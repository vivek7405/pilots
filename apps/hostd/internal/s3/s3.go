// Package s3 is hostd's object-storage client.
//
// Object storage is the ONLY truth for machine state. Local disk is a cache,
// and the design test is that wiping any host's disk loses nothing: a machine
// must be reconstructible on any other host from these objects alone. That is
// what makes cross-host restore and self-heal possible.
//
// The surface is deliberately four operations. Phase 3's content-addressed
// chunk store needs exactly these and nothing more, so keeping it small now
// avoids a client that grows features the engine never uses.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ErrRangeNotSatisfiable reports an HTTP 416.
//
// This is not an edge case to be swallowed: an all-zero diff produces a
// zero-length object, and a range request against it returns 416. Phase 3
// treats that as "these blocks are zeros" rather than an error. Callers must
// be able to tell it apart, so it is surfaced rather than wrapped away.
var ErrRangeNotSatisfiable = errors.New("s3: range not satisfiable")

// ErrNotFound reports a missing key.
var ErrNotFound = errors.New("s3: not found")

// Config describes the bucket and how to reach it.
type Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
}

// Client talks to any S3-compatible object store.
type Client struct {
	api    *s3.Client
	bucket string
	prefix string
}

// New builds a client. Path-style addressing is forced because most
// self-hosted and non-AWS S3 implementations do not serve virtual-host style.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if cfg.AccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load config: %w", err)
	}

	api := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})

	return &Client{api: api, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// key applies the configured prefix.
func (c *Client) key(k string) string {
	if c.prefix == "" {
		return k
	}
	return path.Join(c.prefix, k)
}

// Get fetches a whole object.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := c.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket), Key: aws.String(c.key(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("s3: get %s: %w", key, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("s3: read %s: %w", key, err)
	}
	return data, nil
}

// GetRange fetches a byte range.
//
// A 416 is reported as ErrRangeNotSatisfiable rather than a generic failure:
// the chunk store relies on distinguishing "this object is empty because the
// diff was all zeros" from "this fetch failed".
func (c *Client) GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, nil
	}
	rng := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)

	out, err := c.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket), Key: aws.String(c.key(key)), Range: aws.String(rng),
	})
	if err != nil {
		if isRangeNotSatisfiable(err) {
			return nil, fmt.Errorf("%w: %s %s", ErrRangeNotSatisfiable, key, rng)
		}
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("s3: get range %s %s: %w", key, rng, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("s3: read range %s: %w", key, err)
	}
	return data, nil
}

// Put uploads bytes.
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	_, err := c.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(c.key(key)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	return nil
}

// PutFile streams a file from disk.
//
// Separate from Put because memory images are hundreds of megabytes: reading
// one into a buffer to upload it would multiply hostd's footprint by the
// number of concurrent suspends.
func (c *Client) PutFile(ctx context.Context, key, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("s3: open %s: %w", filePath, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("s3: stat %s: %w", filePath, err)
	}

	_, err = c.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(c.key(key)),
		Body:          f,
		ContentLength: aws.Int64(st.Size()),
	})
	if err != nil {
		return fmt.Errorf("s3: put file %s: %w", key, err)
	}
	return nil
}

// GetToFile streams an object to disk, for the same reason PutFile exists.
func (c *Client) GetToFile(ctx context.Context, key, filePath string) error {
	out, err := c.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket), Key: aws.String(c.key(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return fmt.Errorf("s3: get %s: %w", key, err)
	}
	defer out.Body.Close()

	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("s3: create %s: %w", filePath, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, out.Body); err != nil {
		return fmt.Errorf("s3: write %s: %w", filePath, err)
	}
	return nil
}

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "404")
}

func isRangeNotSatisfiable(err error) bool {
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusRequestedRangeNotSatisfiable {
		return true
	}
	return strings.Contains(err.Error(), "InvalidRange") ||
		strings.Contains(err.Error(), "416")
}
