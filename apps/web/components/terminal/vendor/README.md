# Vendored xterm.js

The only third-party browser code in this app, and it is here rather than in
`package.json` because a WebJs app is buildless: an npm dependency would have to
be resolved by an import map at runtime, and this is two files that never
change between releases.

| File | Package | Version | SHA-256 |
|---|---|---|---|
| `xterm.mjs` | `@xterm/xterm` | 6.0.0 | `b336ec65a086c056d4804b3d4c2347da5663d3f23c3f25be866467bd8857ad59` |
| `addon-fit.mjs` | `@xterm/addon-fit` | 0.11.0 | `2d87e1bddc73be9111de8beee5370c3bb7aac9c94e18e6f245f02ca741ef1769` |
| `../../../public/xterm.css` | `@xterm/xterm` | 6.0.0 | `854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6` |

Both are MIT; `LICENSE` beside this file is the upstream one, unmodified.

## Why this does not cross the framework rule

The rule this repo states is that every web app here is a WebJs app: no other
web framework, bundler or UI library. xterm.js is none of those. It is a
terminal emulator, which is a VT sequence parser plus a canvas renderer, and
there is no version of writing one of those by hand that is a good use of
anyone's time. It ships to one route, `/machines/[id]/terminal`, and nothing
else in the browser comes from outside this repo.

## Updating

```sh
npm pack @xterm/xterm@<version> @xterm/addon-fit@<version>
tar xzf xterm-xterm-<version>.tgz
cp package/lib/xterm.mjs        components/terminal/vendor/xterm.mjs
cp package/css/xterm.css        public/xterm.css
cp package/LICENSE              components/terminal/vendor/LICENSE
tar xzf xterm-addon-fit-<version>.tgz
cp package/lib/addon-fit.mjs    components/terminal/vendor/addon-fit.mjs
npm run css:build
sha256sum components/terminal/vendor/*.mjs public/xterm.css   # update the table above
```

The files are copied BYTE FOR BYTE, sourcemap comments included, so the table
above is checkable with one command and a modification is impossible to miss.
The `.map` files are deliberately not vendored: a browser fetches one only when
devtools is open and asks for it, and 1.5 MB of source maps in the repo to save
that one 404 is a bad trade.

`addon-fit.mjs` imports nothing, so no import map entry is needed. The
component imports both files by relative path, which is what a buildless app
serves directly.
