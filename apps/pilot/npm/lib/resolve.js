'use strict';
/**
 * Which binary this machine runs, or why there is none.
 *
 * The package carries EVERY platform's binary in vendor/, under the release
 * asset's own name (`pilot_<goos>_<goarch>`, the contract shared with
 * .github/workflows/release-pilot.yml, apps/web/public/install.sh and
 * `pilot upgrade`). One package rather than one per platform: it costs a
 * larger download and buys a single thing to publish, with nothing resting on
 * how a given npm version resolves optionalDependencies. No install script
 * downloads anything, ever: npm 12 turns install scripts off by default, and
 * a CLI that works only when they are on is a CLI that stops working.
 *
 * Pure and synchronous so the test can ask about platforms it is not on.
 */
const path = require('node:path');

/** Node's names on the left, Go's (which the assets are named with) on the right. */
const GOOS = { linux: 'linux', darwin: 'darwin' };
const GOARCH = { x64: 'amd64', arm64: 'arm64' };

const INSTALL_PAGE = 'https://pilots.run/install';

/**
 * @param {string} platform  process.platform
 * @param {string} arch      process.arch
 * @param {string} [vendorDir]
 * @returns {{ binary: string } | { error: string }}
 */
function binaryFor(platform, arch, vendorDir = path.join(__dirname, '..', 'vendor')) {
  if (platform === 'win32') {
    // Said here, by name, because the alternative is a Windows user watching
    // an install succeed and then a command fail with ENOENT on a path inside
    // node_modules, which explains nothing.
    return {
      error: [
        'pilot has no Windows build.',
        '',
        'It runs under WSL, which is Linux: open a WSL shell and run the same',
        'command there (npm install -g pilots), or the curl installer.',
        '',
        `  ${INSTALL_PAGE}`,
        '  https://github.com/pilotsrun/pilots/issues/135 tracks native Windows support.',
      ].join('\n'),
    };
  }
  const goos = GOOS[platform];
  const goarch = GOARCH[arch];
  if (!goos || !goarch) {
    return {
      error: [
        `pilot has no build for ${platform}/${arch}.`,
        '',
        'It is built for linux and darwin, on x64 and arm64. From source:',
        '  git clone https://github.com/pilotsrun/pilots && cd pilots/apps/pilot && go build ./cmd/pilot',
      ].join('\n'),
    };
  }
  return { binary: path.join(vendorDir, `pilot_${goos}_${goarch}`) };
}

module.exports = { binaryFor };
