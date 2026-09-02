// Package certs backs certmagic with the fleet's object storage, so any host
// can answer a TLS handshake for any name.
package certs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/vivek7405/pilots/hostd/internal/s3"
)

// Storage is certmagic.Storage over the same bucket everything else uses.
//
// Shared storage is not a convenience here, it is what makes ACME work at all
// on a fleet with no coordinator. Wildcard DNS points *.pilotrun.app at EVERY
// host, so an HTTP-01 challenge for a custom domain lands on whichever host
// the client resolved -- almost never the one that started the order. certmagic
// solves that with a distributed solver that writes the challenge token to
// Storage, and every host answers from there. With per-host storage the
// challenge fails (N-1)/N of the time on an N-host fleet, which looks like
// flaky Let's Encrypt rather than a configuration mistake.
//
// It is also why the lock below has to be real.
type Storage struct {
	s3     *s3.Client
	prefix string
	// hostID identifies who holds a lock, so a stuck one names the host to go
	// look at rather than just a timestamp.
	hostID string
}

// New wraps a client that is ALREADY scoped to the certificate area.
//
// No prefix is added here: the caller builds the client with one, the way
// every other store in the process is built, and adding a second would put
// certificates under certs/certs/ -- consistent, so nothing would break, and
// wrong in a way that only shows up when someone goes looking in the bucket.
func New(client *s3.Client, hostID string) *Storage {
	return &Storage{s3: client, hostID: hostID}
}

func (st *Storage) objectKey(key string) string {
	if st.prefix == "" {
		return key
	}
	return path.Join(st.prefix, key)
}

func (st *Storage) Store(ctx context.Context, key string, value []byte) error {
	return st.s3.Put(ctx, st.objectKey(key), value)
}

func (st *Storage) Load(ctx context.Context, key string) ([]byte, error) {
	b, err := st.s3.Get(ctx, st.objectKey(key))
	if err != nil {
		// certmagic tests for this exact sentinel to decide "no certificate
		// yet, go get one" versus "storage is broken, do not touch anything".
		// Returning a generic error here turns a first-time issue into a
		// hard failure.
		if errors.Is(err, s3.ErrNotFound) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	return b, nil
}

func (st *Storage) Delete(ctx context.Context, key string) error {
	// A directory delete has to remove everything under it: certmagic deletes
	// an asset by its logical name and expects the whole subtree to go.
	objs, err := st.s3.List(ctx, st.objectKey(key)+"/")
	if err == nil && len(objs) > 0 {
		for _, o := range objs {
			if err := st.s3.Delete(ctx, o.Key); err != nil {
				return err
			}
		}
		return nil
	}
	return st.s3.Delete(ctx, st.objectKey(key))
}

func (st *Storage) Exists(ctx context.Context, key string) bool {
	if _, err := st.s3.Get(ctx, st.objectKey(key)); err == nil {
		return true
	}
	objs, err := st.s3.List(ctx, st.objectKey(key)+"/")
	return err == nil && len(objs) > 0
}

func (st *Storage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	full := st.objectKey(prefix)
	objs, err := st.s3.List(ctx, full+"/")
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	var out []string
	for _, o := range objs {
		rel := o.Key
		if st.prefix != "" {
			rel = strings.TrimPrefix(rel, st.prefix+"/")
		}
		if recursive {
			out = append(out, rel)
			continue
		}
		// Non-recursive means immediate children only, with a "directory"
		// reported once rather than once per object beneath it.
		base := full
		if st.prefix != "" {
			base = strings.TrimPrefix(full, st.prefix+"/")
		}
		tail := strings.TrimPrefix(rel, base+"/")
		if i := strings.Index(tail, "/"); i >= 0 {
			tail = tail[:i]
		}
		child := path.Join(prefix, tail)
		if _, dup := seen[child]; dup {
			continue
		}
		seen[child] = struct{}{}
		out = append(out, child)
	}
	return out, nil
}

func (st *Storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	objs, err := st.s3.List(ctx, st.objectKey(key))
	if err != nil {
		return certmagic.KeyInfo{}, err
	}
	for _, o := range objs {
		if o.Key == st.objectKey(key) {
			return certmagic.KeyInfo{
				Key: key, Modified: o.Modified, Size: o.Size, IsTerminal: true,
			}, nil
		}
	}
	if len(objs) > 0 {
		return certmagic.KeyInfo{Key: key, IsTerminal: false}, nil
	}
	return certmagic.KeyInfo{}, fs.ErrNotExist
}

// lockInfo is what a held lock records, so an operator looking at a stuck
// order can see which host to go and check.
type lockInfo struct {
	Host    string    `json:"host"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

// lockTTL bounds how long a lock survives its holder.
//
// A host that dies mid-order must not block every other host from ever issuing
// that certificate again, and there is no coordinator to notice it died. The
// TTL is the whole recovery mechanism, so it is short enough to matter and
// long enough that a slow ACME round trip does not lose its own lock.
const lockTTL = 5 * time.Minute

// pollInterval is how often a waiter re-checks. Object storage has no watch,
// so this is a poll by necessity rather than by choice.
const pollInterval = 2 * time.Second

func (st *Storage) Lock(ctx context.Context, name string) error {
	key := st.objectKey("locks/" + name + ".json")
	for {
		raw, err := st.s3.Get(ctx, key)
		switch {
		case errors.Is(err, s3.ErrNotFound):
			// Genuinely absent. Take it.
			if err := st.tryTake(ctx, key); err == nil {
				return nil
			}
			// Lost the race to another host writing the same key. Fall through
			// and wait rather than proceeding: two holders is the duplicate
			// issuance this lock exists to prevent.
		case err != nil:
			// A read that FAILED is not a free lock. Treating it as one turns
			// a transient bucket error into every host issuing at once, which
			// spends the fleet's shared Let's Encrypt rate limit. Wait and ask
			// again; ctx bounds how long.
		default:
			var info lockInfo
			if json.Unmarshal(raw, &info) != nil || time.Now().After(info.Expires) {
				// Expired or unreadable: the holder is gone, or never wrote a
				// usable record. Either way it must not block the fleet
				// forever -- there is no coordinator to notice it died.
				if err := st.tryTake(ctx, key); err == nil {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// tryTake claims the lock and confirms it is the holder.
//
// Object storage gives no compare-and-set, so this writes and then reads back
// to see whose record survived. Last-writer-wins means exactly one host reads
// its own id back, and the others see someone else's and keep waiting -- which
// is the property the lock needs, without pretending the bucket offers an
// atomic primitive it does not.
//
// The read-back is deliberately after a short settle: an immediately
// consistent store returns the winner straight away, and an eventually
// consistent one is given a moment rather than being trusted blindly.
func (st *Storage) tryTake(ctx context.Context, key string) error {
	if err := st.writeLock(ctx, key); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(250 * time.Millisecond):
	}

	raw, err := st.s3.Get(ctx, key)
	if err != nil {
		return err
	}
	var info lockInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return err
	}
	if info.Host != st.hostID {
		return fmt.Errorf("certs: %s holds the lock", info.Host)
	}
	return nil
}

func (st *Storage) writeLock(ctx context.Context, key string) error {
	now := time.Now()
	raw, err := json.Marshal(lockInfo{Host: st.hostID, Created: now, Expires: now.Add(lockTTL)})
	if err != nil {
		return err
	}
	return st.s3.Put(ctx, key, raw)
}

func (st *Storage) Unlock(ctx context.Context, name string) error {
	return st.s3.Delete(ctx, st.objectKey("locks/"+name+".json"))
}

var _ certmagic.Storage = (*Storage)(nil)
