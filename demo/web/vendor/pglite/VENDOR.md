# Vendored: PGlite, PostgreSQL 19 fork

This directory is a **pinned, unmodified** copy of a published release. Nothing
in it is hand-written and nothing in it should ever be edited.

| | |
|---|---|
| package | `@electric-sql/pglite` |
| version | `0.5.4-pg19.1` |
| repo | `gaarutyunov/pglite` |
| release tag | `pglite-v0.5.4-pg19.1` (**pre-release** — pinned by tag, deliberately) |
| asset | `electric-sql-pglite-0.5.4-pg19.1.tgz` |
| sha256 | `b4d0531251bc90f17e5e113605b6540b9e7585a0265bfd3e8456f7ec7999f9fb` |
| engine | PostgreSQL 19beta2 |
| pglite commit | `a9ec5f193623985e3f5378a0240457c0f4860997` |
| postgres-pglite commit | `f13a3a52cde9f65eea0e13857a806ca6ea99ec46` |

## Why it is vendored rather than fetched

The npm package lives on GitHub Packages, which requires authentication even
for public packages, and a static Pages build has no credentials to give it.
Release assets download anonymously, so the tarball is fetched once and its
contents committed. A CDN was not considered: the deployed tree on `gh-pages`
is meant to be inspectable, and a third-party origin is a dependency the demo
would acquire at *run* time rather than at build time.

## Why it is 17 MB and not the 9 MB `.wasm`

PGlite's emscripten glue checks `pglite.data`'s byte length against a value
baked in at link time (`gaarutyunov/postgres-pglite#28`), so the published
upstream glue rejects any FS bundle but its own. The fork's matching glue and
the separate `initdb` module have to ship with it. Vendoring only
`pglite.wasm` + `pglite.data` produces a demo that cannot boot.

The four payloads that dominate the size:

| file | bytes | what it is |
|---|---|---|
| `pglite.wasm` | 9 379 167 | the PostgreSQL 19 engine |
| `pglite.data` | 5 434 131 | its emscripten FS bundle (share/, timezones, initdb templates) |
| `pglite.js` | 457 223 | the fork's matching emscripten glue |
| `initdb.wasm` + `initdb.js` | 171 989 + 108 261 | the separate initdb module |

`index.js` (413 KB) plus six `chunk-*.js` files are the ES module entry point;
the emscripten glue is bundled into them, which is why `index.js` never
references `pglite.js` by name. Both standalone glue files are kept anyway —
they are 0.5 MB against 17, and `postgres-pglite#28`'s constraint is about the
glue matching the data, which is not a thing to be clever about.

The 38 `*.tar.gz` files and the `contrib/` loaders are PostgreSQL extension
bundles (1.5 MB total). M1 loads none of them; they are kept because they are
part of what "the complete bundle" means and because an extension added in a
later milestone should not require re-vendoring.

## What was excluded

Source maps (`*.map`), the CommonJS build (`*.cjs`) and TypeScript declarations
(`*.d.ts`, `*.d.cts`). The demo loads the ES module entry point in a browser;
none of the three is reachable from it. That is what takes the directory from
23 MB to 17 MB.

## Re-vendoring

```sh
gh release download pglite-v0.5.4-pg19.1 --repo gaarutyunov/pglite \
  --pattern 'electric-sql-pglite-*.tgz' --dir /tmp/pglite
cd /tmp/pglite && shasum -a 256 -c SHA256SUMS && tar -xzf electric-sql-pglite-*.tgz

cd /tmp/pglite/package/dist
find . -type f ! -name '*.map' ! -name '*.cjs' ! -name '*.d.ts' ! -name '*.d.cts' -print0 \
  | tar --null -cf - -T - | (cd <repo>/demo/web/vendor/pglite && tar -xf -)
```

Bumping the version means updating the table above **and** `PGLITE_VERSION` in
`demo/web/boot.js`, which is what the page reports.
