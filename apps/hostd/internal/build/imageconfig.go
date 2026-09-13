package build

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
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

// readImageConfig reads buildctl's metadata file and returns the base image's
// config, or nil when the build published none.
func readImageConfig(path string) (*ImageConfig, error) {
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
