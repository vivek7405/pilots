package build

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/s3"
)

// CacheStore is object storage for the layer cache, as hostd addresses it.
//
// hostd holds these credentials. The BuildKit daemon never does, and that is
// the point: it runs inside a machine the ORG controls, and an org that could
// write cache objects could write a manifest under any key for the next org
// whose Dockerfile hashed the same to import. So the daemon writes to a
// directory on this host, over the buildctl session, and hostd is the only
// thing that moves those bytes to or from a bucket.
type CacheStore interface {
	PutFile(ctx context.Context, key, filePath string) error
	GetToFile(ctx context.Context, key, filePath string) error
	List(ctx context.Context, prefix string) ([]s3.ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

// sharedSeedDir is the one cache entry hostd writes itself, and the only one
// any org may import without having produced it.
//
// It holds the base layers every Node application starts from, built here from
// seedDockerfile with no customer input at all. That is what makes it safe to
// share: nothing an org controls ever reaches it.
const sharedSeedDir = "shared/node-base"

// seedDockerfile is the context-independent prefix of the webjs scaffold
// recipe in internal/detect.
//
// Deliberately only the prefix. The steps after it copy the application's
// package.json and run npm install, and BuildKit keys those on the copied
// file's content, so seeding them from a package.json no real app has would
// produce entries nothing ever hits. What every Node app on this host DOES
// share is the base image pull and the certificate layer, which is the
// expensive part of a cold build, and that is exactly what this warms.
//
// Exported so detect can assert it stays a prefix of the real recipe. That
// assertion lives there, in TestTheSharedCacheSeedMatchesTheRecipe, because
// detect imports this package and not the other way round.
const SeedDockerfile = `FROM node:24-alpine
RUN apk add --no-cache ca-certificates
`

// maxCacheEntriesPerOrg bounds how many Dockerfile partitions an org keeps.
//
// A measured mode=max export of a Node base image plus an npm install is about
// 73 MiB, so eight is a few hundred megabytes per org per host. The bound is on
// COUNT rather than bytes because the unbounded axis is how many different
// Dockerfiles an org builds, not how big one of them gets.
const maxCacheEntriesPerOrg = 8

// cacheDir is where the daemon exports one org's cache for one Dockerfile.
//
// Empty for an org-less build, which disables the cache entirely rather than
// pooling unowned builds together. An admin-key build with no org must not be
// able to write anything another tenant reads, and "no cache" is the only
// answer that is obviously safe.
func (b *Builder) cacheDir(orgID, cacheName string) string {
	if b.opts.CacheDir == "" || orgID == "" || cacheName == "" {
		return ""
	}
	return filepath.Join(b.opts.CacheDir, "orgs", sanitizeKey(orgID), cacheName)
}

// seedDir is the shared read-only import, or "" when this host has not seeded.
func (b *Builder) seedDir() string {
	if b.opts.CacheDir == "" {
		return ""
	}
	dir := filepath.Join(b.opts.CacheDir, sharedSeedDir)
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		return ""
	}
	return dir
}

// cacheKeyPrefix is where a local cache directory is mirrored in the bucket.
func cacheKeyPrefix(orgID, cacheName string) string {
	return "orgs/" + sanitizeKey(orgID) + "/" + cacheName + "/"
}

// sanitizeKey keeps an id usable as one path segment in both a filesystem and
// an object key. Ids here are already opaque and flat, so this is a guard
// against a future id format rather than work it does today.
func sanitizeKey(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

// pullCache fetches an org's cache for one Dockerfile if this host has none.
//
// A miss is not an error. S3 is the truth and this disk is a cache, so a host
// whose build-cache was wiped pays one download; a host whose org has never
// built pays nothing and builds cold. Neither may fail the build, which is why
// every error here is logged and swallowed.
func (b *Builder) pullCache(ctx context.Context, dir, orgID, cacheName string) {
	if dir == "" || b.opts.CacheStore == nil {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
		// Already warm locally. Stamp it so the prune keeps what is in use.
		_ = os.Chtimes(dir, time.Now(), time.Now())
		return
	}

	prefix := cacheKeyPrefix(orgID, cacheName)
	objects, err := b.opts.CacheStore.List(ctx, prefix)
	if err != nil {
		slog.Warn("could not list the layer cache; building cold",
			"org", orgID, "cache", cacheName, "err", err)
		return
	}
	if len(objects) == 0 {
		return
	}
	start := time.Now()
	for _, o := range objects {
		rel := strings.TrimPrefix(o.Key, prefix)
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			slog.Warn("could not make room for the layer cache; building cold", "err", err)
			return
		}
		if err := b.opts.CacheStore.GetToFile(ctx, o.Key, dest); err != nil {
			slog.Warn("could not fetch the layer cache; building cold",
				"key", o.Key, "err", err)
			// A half-fetched cache is worse than none: BuildKit would read a
			// manifest naming blobs that are not there.
			_ = os.RemoveAll(dir)
			return
		}
	}
	slog.Info("pulled the layer cache", "org", orgID, "cache", cacheName,
		"objects", len(objects), "seconds", int(time.Since(start).Seconds()))
}

// pushCache mirrors a freshly exported cache directory to object storage, so
// that any host can warm this org's next build. Best effort for the same
// reason as pullCache: the image is already built and published, and failing
// the build now would throw that away over a cache.
func (b *Builder) pushCache(ctx context.Context, dir, orgID, cacheName string) {
	if dir == "" || b.opts.CacheStore == nil {
		return
	}
	prefix := cacheKeyPrefix(orgID, cacheName)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		return b.opts.CacheStore.PutFile(ctx, prefix+filepath.ToSlash(rel), path)
	})
	if err != nil {
		slog.Warn("could not mirror the layer cache; the next build on another host "+
			"will be colder", "org", orgID, "cache", cacheName, "err", err)
		return
	}
	b.pruneCache(ctx, orgID)
}

// pruneCache keeps the most recently used entries for one org and drops the
// rest, locally and in the bucket.
func (b *Builder) pruneCache(ctx context.Context, orgID string) {
	root := filepath.Join(b.opts.CacheDir, "orgs", sanitizeKey(orgID))
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) <= maxCacheEntriesPerOrg {
		return
	}
	type aged struct {
		name string
		mod  time.Time
	}
	var dirs []aged
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, aged{e.Name(), info.ModTime()})
	}
	if len(dirs) <= maxCacheEntriesPerOrg {
		return
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].mod.After(dirs[j].mod) })

	for _, d := range dirs[maxCacheEntriesPerOrg:] {
		if err := os.RemoveAll(filepath.Join(root, d.name)); err != nil {
			slog.Warn("could not prune a local cache entry", "dir", d.name, "err", err)
			continue
		}
		prefix := cacheKeyPrefix(orgID, d.name)
		objects, err := b.opts.CacheStore.List(ctx, prefix)
		if err != nil {
			continue
		}
		for _, o := range objects {
			if err := b.opts.CacheStore.Delete(ctx, o.Key); err != nil {
				slog.Warn("could not prune a mirrored cache object", "key", o.Key, "err", err)
			}
		}
		slog.Info("pruned a layer cache entry past the per-org bound",
			"org", orgID, "cache", d.name)
	}
}

// SeedSharedCache builds the base layers every Node application starts from
// and leaves them where every build imports them read-only.
//
// This is the ONLY writer of the shared entry, and it has to be: an org's
// daemon produced the bytes in that org's own cache, so promoting one org's
// output into a directory every other org imports would let a tenant choose
// what its neighbours build on. Here the input is a constant in this package
// and the builder is created with no org at all.
//
// Runs once per host, in the background, and a failure is logged rather than
// fatal: without it builds are colder, not broken.
func (b *Builder) SeedSharedCache(ctx context.Context) {
	if b.opts.CacheDir == "" || b.opts.Builders == nil {
		return
	}
	dir := filepath.Join(b.opts.CacheDir, sharedSeedDir)
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
		return
	}

	// The bucket may already hold it from another host on this fleet.
	if b.opts.CacheStore != nil {
		if objects, err := b.opts.CacheStore.List(ctx, sharedSeedDir+"/"); err == nil && len(objects) > 0 {
			b.pullSeed(ctx, dir, objects)
			if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
				return
			}
		}
	}

	work, err := os.MkdirTemp(b.opts.WorkRoot, "seed-")
	if err != nil {
		slog.Warn("could not seed the shared layer cache", "err", err)
		return
	}
	defer os.RemoveAll(work)

	ctxDir := filepath.Join(work, "context")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		slog.Warn("could not seed the shared layer cache", "err", err)
		return
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(SeedDockerfile), 0o644); err != nil {
		slog.Warn("could not seed the shared layer cache", "err", err)
		return
	}

	// No org: the platform's own builder, holding no tenant context.
	addr, release, err := b.opts.Builders.EnsureBuilder(ctx, "")
	if err != nil {
		slog.Warn("could not start a builder to seed the shared layer cache", "err", err)
		return
	}
	defer release()

	start := time.Now()
	args := []string{
		"--addr", addr, "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + ctxDir,
		"--local", "dockerfile=" + ctxDir,
		"--output", "type=tar,dest=" + filepath.Join(work, "discard.tar"),
		"--progress", "rawjson",
		"--export-cache", "type=local,dest=" + dir + ",mode=max",
	}
	if _, err := b.run(ctx, b.opts.BuildctlBin, args...); err != nil {
		slog.Warn("seeding the shared layer cache failed; builds will be colder", "err", err)
		_ = os.RemoveAll(dir)
		return
	}
	slog.Info("seeded the shared layer cache", "seconds", int(time.Since(start).Seconds()))

	if b.opts.CacheStore != nil {
		if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, rerr := filepath.Rel(dir, path)
			if rerr != nil {
				return rerr
			}
			return b.opts.CacheStore.PutFile(ctx, sharedSeedDir+"/"+filepath.ToSlash(rel), path)
		}); err != nil {
			slog.Warn("could not mirror the shared layer cache", "err", err)
		}
	}
}

// pullSeed fetches a shared seed another host already built.
func (b *Builder) pullSeed(ctx context.Context, dir string, objects []s3.ObjectInfo) {
	prefix := sharedSeedDir + "/"
	for _, o := range objects {
		rel := strings.TrimPrefix(o.Key, prefix)
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return
		}
		if err := b.opts.CacheStore.GetToFile(ctx, o.Key, dest); err != nil {
			slog.Warn("could not fetch the shared layer cache", "key", o.Key, "err", err)
			_ = os.RemoveAll(dir)
			return
		}
	}
}
