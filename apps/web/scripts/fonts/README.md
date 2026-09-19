# Fonts for the social card

`inter.woff2`, `inter-tight.woff2` and `jetbrains-mono.woff2` are read by
`../build-og.mjs` and inlined into the page it screenshots. They are here, and
not under `public/`, because the site serves no fonts: these exist only so the
card renders the same on whichever machine runs the script.

All three are under the SIL Open Font License 1.1, which permits bundling and
redistribution: Inter and Inter Tight by Rasmus Andersson
(https://github.com/rsms/inter), JetBrains Mono by JetBrains
(https://github.com/JetBrains/JetBrainsMono).
