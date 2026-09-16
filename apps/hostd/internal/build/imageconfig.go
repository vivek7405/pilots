package build

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Recovering the base image's start command from a build.
//
// The tar exporter emits a flattened filesystem and nothing else: no CMD, no
// ENTRYPOINT, no ENV, no WORKDIR, no USER. That is the right trade for an
// engine that boots a rootfs rather than running a container, but it means a
// compose file saying `image: postgres:17` used to build cleanly and then have
// nothing to execute. The Dockerfile parser cannot fill the gap either, since
// what is missing is precisely what the BASE image declares.
//
// buildctl's --metadata-file is where the frontend publishes what it already
// resolved, so the config is read from there rather than by pulling the image
// a second time on the host. The value under containerimage.config is the
// base64 of the image's config JSON, whose "config" object is the part that
// says how to start it.
//
// Read defensively on purpose. Which keys an exporter publishes is a property
// of the buildkit version the builder image pins, and a build that produced a
// correct filesystem must not fail because a key moved: a missing or
// unparseable config falls back to the Dockerfile alone, which is exactly the
// behaviour every build had before this existed, and the spec records it as
// FromDockerfileOnly so the difference is visible rather than guessed at.

// imageConfigKey is buildkit's own metadata key (exptypes.ExporterImageConfigKey).
const imageConfigKey = "containerimage.config"

// imageManifest is the shape of an OCI image config document. Only the config
// object is read; rootfs and history say nothing about starting the image.
type imageManifest struct {
	Config ImageConfig `json:"config"`
}

// ociLayoutDir is where the build's OCI layout export lands, beside the
// metadata file it accompanies.
func ociLayoutDir(metadataPath string) string {
	return strings.TrimSuffix(metadataPath, filepath.Ext(metadataPath)) + ".oci"
}

// readImageConfig returns the base image's config, or nil when the build
// published none.
//
// Two sources, in order of how much they can be trusted not to move:
//
//  1. The OCI layout the solve exports beside the metadata file. This is an
//     OCI spec artifact -- index.json, a manifest, a config blob -- so it says
//     the same thing on every buildkit version.
//  2. buildkit's own metadata key, for a version that publishes the config
//     inline. None currently does; 0.32 publishes only the DIGEST of it,
//     which is why reading the metadata alone recovered nothing for as long
//     as this code has existed.
func readImageConfig(path string) (*ImageConfig, error) {
	if cfg, err := configFromOCILayout(ociLayoutDir(path)); err == nil && cfg != nil {
		return cfg, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read build metadata: %w", err)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("parse build metadata: %w", err)
	}
	entry, ok := meta[imageConfigKey]
	if !ok {
		return nil, nil
	}
	// The value is a JSON string holding base64. A future buildkit publishing
	// the object inline rather than encoded is handled too, because the cost
	// of the second attempt is one failed unmarshal.
	var encoded string
	if err := json.Unmarshal(entry, &encoded); err != nil {
		var inline imageManifest
		if err2 := json.Unmarshal(entry, &inline); err2 != nil {
			return nil, fmt.Errorf("parse %s: %w", imageConfigKey, err)
		}
		return &inline.Config, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", imageConfigKey, err)
	}
	var manifest imageManifest
	if err := json.Unmarshal(decoded, &manifest); err != nil {
		return nil, fmt.Errorf("parse the image config: %w", err)
	}
	return &manifest.Config, nil
}

// ociDescriptor is the one field of an OCI descriptor this needs: where the
// blob it points at lives.
type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}

// ociIndexOrManifest is both shapes at once, because which one a layout's
// index.json points at depends on whether the build was single-platform.
// Exactly one of the two fields is populated in a valid document.
type ociIndexOrManifest struct {
	Manifests []ociDescriptor `json:"manifests"` // an index
	Config    *ociDescriptor  `json:"config"`    // a manifest
}

// configFromOCILayout walks dir's index.json down to the image config blob.
//
// index.json -> (optionally another index) -> a manifest -> its config. The
// walk is bounded: a layout that points at itself would otherwise hang a
// build, and no legitimate one is more than two levels deep.
func configFromOCILayout(dir string) (*ImageConfig, error) {
	blob := func(d ociDescriptor) ([]byte, error) {
		hex, ok := strings.CutPrefix(d.Digest, "sha256:")
		if !ok {
			return nil, fmt.Errorf("unsupported digest %q", d.Digest)
		}
		// Not path.Join with attacker input: the hex is checked to be one
		// path segment so a digest cannot climb out of the blob store.
		if hex == "" || strings.ContainsAny(hex, `/\.`) {
			return nil, fmt.Errorf("malformed digest %q", d.Digest)
		}
		return os.ReadFile(filepath.Join(dir, "blobs", "sha256", hex))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	for depth := 0; depth < 4; depth++ {
		var doc ociIndexOrManifest
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse the OCI layout: %w", err)
		}
		if doc.Config != nil {
			cfgRaw, err := blob(*doc.Config)
			if err != nil {
				return nil, fmt.Errorf("read the image config blob: %w", err)
			}
			var manifest imageManifest
			if err := json.Unmarshal(cfgRaw, &manifest); err != nil {
				return nil, fmt.Errorf("parse the image config: %w", err)
			}
			return &manifest.Config, nil
		}
		if len(doc.Manifests) == 0 {
			return nil, fmt.Errorf("the OCI layout names no manifest")
		}
		if raw, err = blob(doc.Manifests[0]); err != nil {
			return nil, fmt.Errorf("read an OCI manifest: %w", err)
		}
	}
	return nil, fmt.Errorf("the OCI layout nests deeper than any real image")
}
