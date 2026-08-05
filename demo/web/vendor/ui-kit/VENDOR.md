# Vendored: `gaarutyunov/ui-kit`

Pinned, unmodified release assets. Do not edit.

| | |
|---|---|
| package | `@gaarutyunov/ui-kit` |
| release tag | `v0.3.0` |
| assets | `ga-ui-kit.css`, `ga-ui-kit.min.js` |

## Why vendored, and why the release assets rather than npm

The package is published to GitHub Packages, which requires a token with
`read:packages` **even for public packages**. A Pages build has no such token,
and `GITHUB_TOKEN` cannot grant it. Release assets, by contrast, download
anonymously. So the two files are fetched once from a pinned tag and committed.

`v0.3.0` is the latest published release. It is pinned by tag rather than
tracked through `releases/latest/download/…`, because a new ui-kit release
would otherwise land on the deployed demo with no change on this side.

## Loading them

`ga-ui-kit.css` is a plain stylesheet and loads through `<link>`.
`ga-ui-kit.min.js` is a classic script that registers every `<ga-*>` custom
element on load, so it goes in a `<script>` — not an import, because a bare
specifier does not resolve in a browser without a bundler and there is no
bundler here (SPEC.md §13.4). An ES-module build (`ga-ui-kit.esm.js`) is
attached to the same release for the pages that want one; this one does not.

## Re-vendoring

```sh
gh release download v0.3.0 --repo gaarutyunov/ui-kit \
  --pattern 'ga-ui-kit.css' --pattern 'ga-ui-kit.min.js' \
  --dir <repo>/demo/web/vendor/ui-kit --clobber
```
