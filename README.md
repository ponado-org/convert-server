# convert-server

Self-hosted conversion service: Typst -> PDF, images -> browser-safe images,
PDFs -> preview images, and Office documents -> PDF via Gotenberg.

One small Go service wrapping the prebuilt `typst` and `vips` CLIs. Clients
POST a file (or a Typst template) and get bytes back that a browser can display
inline, so no project has to bundle a converter into its server build.

```
                    ┌─ typst      template -> PDF
client --HTTP POST──┤
                    ├─ vips       images / PDF / SVG -> png, jpg, webp
                    └─ gotenberg  docx, odt, ... -> PDF (optional)

                    all backends -> browser-viewable bytes
```

## Supported formats

| Input | Formats | Backend |
| ----- | ------- | ------- |
| Images | `png`, `jpg`/`jpeg`, `gif`, `webp`, `tiff`/`tif`, `heic`/`heif`, `avif`, `jxl`, `svg`/`svgz`, `bmp`, `ico`, `psd`, `tga` | vips (optional) |
| PDF | `pdf` (pass through or rasterise a page) | vips / poppler |
| Office & text | `doc`, `docx`, `docm`, `dot`, `dotx`, `odt`, `ott`, `rtf`, `txt`, `csv`, `tsv`, `xls`, `xlsx`, `xlsm`, `ods`, `ots`, `ppt`, `pptx`, `pptm`, `odp`, `otp` | Gotenberg (optional) |
| HTML | `html` | Gotenberg (optional) |
| Typst | templates via `POST /typst/render` | typst (always) |
| Output | `pdf`, `png`, `jpg`, `webp` | |

Image conversion requires libvips; Office/HTML requires Gotenberg. A
Typst-only build can omit both (see [image variants](#image-variants)).

Smart defaults when `to` is omitted:

- images become **PNG**, except formats browsers already display (`png`, `jpg`,
  `gif`, `webp`) which are returned untouched - GIF keeps its animation;
- PDFs are returned untouched;
- documents become **PDF**.

SVG is deliberately rasterised to PNG rather than served inline, so untrusted
SVG cannot run script on this service's origin.

## Quick start

```bash
docker compose up -d                                # pull the prebuilt image, serve on :3100
docker compose -f compose.build.yml up -d --build   # or build the local tree instead
curl -s localhost:3100/health                       # ok
```

`compose.yml` is the copy-pasteable, production one: it only pulls the
published image (`CONVERT_IMAGE` overrides the registry/tag). Published to GHCR:

```bash
docker compose up -d
CONVERT_IMAGE=ghcr.io/ponado-org/convert-server:latest docker compose up -d
CONVERT_IMAGE=ghcr.io/ponado-org/convert-server:typst docker compose up -d
```

`compose.build.yml` is the local-build variant used during development.

### Image variants and tags

Variants are **tags in the same repository**, not separate repositories. One
version, one release pipeline, and clients can pin either a moving variant tag
or an immutable `version+variant` tag. Separate repos are only worth it if the
variants need independent access control or a different release cadence.

| Variant | Mode | Build args | Published tags | Size |
| ------- | ---- | ---------- | -------------- | ---- |
| Full | `prod` | defaults | `vX.Y.Z`, `vX.Y`, `latest`, `sha-<short>` | ~233 MB |
| Core | `core` | `WITH_VIPS_EXTRAS=false` | `vX.Y.Z-core`, `core` | ~221 MB |
| Typst-only | `typst` | `WITH_VIPS=false` | `vX.Y.Z-typst`, `typst` | ~120 MB |

`latest` always means **full**. Variant builds set `latest: false` and use
`--no-sha-tag`, so they never overwrite the full image's `sha-<short>` commit
tag. Sizes are approximate local `docker images` sizes; pulls are smaller.

Capabilities:

- **Full** - everything in the format table.
- **Core** - HEIC/TIFF/WebP/PDF/SVG etc.; drops JPEG XL, BMP, ICO, PSD, TGA.
- **Typst-only** - `/typst/render`, PDF pass-through, PNG/JPG/GIF/WebP
  pass-through, and Office->PDF if Gotenberg is configured. Image transcoding
  returns `501`.

Named modes live in `package.json` → `dockerImage.modes`, so the tag/platform
policy stays in one place. The convenience scripts are thin wrappers:

```bash
pnpm install                 # once; installs @ponado-org/docker-image

# local builds -> convert-server:full / :core / :typst
pnpm build:full
pnpm build:core
pnpm build:typst
pnpm build:all

# raw Docker / compose equivalents
docker build -t convert-server:full .
docker build --build-arg WITH_VIPS_EXTRAS=false -t convert-server:core .
docker build --build-arg WITH_VIPS=false -t convert-server:typst .
WITH_VIPS=false docker compose -f compose.build.yml up -d --build
```

Publish a variant (multi-arch, GHCR):

```bash
pnpm exec ponado-docker-image --mode core --no-sha-tag
pnpm exec ponado-docker-image --mode typst --no-sha-tag

# inspect the exact tags/commands without pushing
pnpm exec ponado-docker-image --mode core --no-sha-tag --dry-run
pnpm exec ponado-docker-image --mode typst --no-sha-tag --dry-run
```

Releases are built by GitHub Actions on `v*` tag pushes
(`.github/workflows/docker.yml`). The workflow publishes the `core` and `typst`
variants first, then the full image, so `latest` only moves after every variant
succeeded.

```bash
pnpm release:patch     # or release:minor / release:major / release
```

This bumps the version, commits it, and pushes `main` plus the tag. The tag push
starts the workflow; a manual run can target `all`, `prod`, `core` or `typst`.

If Actions is unavailable, build the same images locally from the tagged HEAD:

```bash
pnpm release:images    # variants first, then prod
```

The full `prod` build refuses a dirty tree, a non-`main` branch, or a HEAD that
is not tagged `v<version>`. CI checks out the tag, so the same policy applies
there. `pnpm release:variants` builds only the two variants.

Ad-hoc tags go straight to the CLI:

```bash
# an extra moving tag alongside v0.2.0-typst and typst
pnpm exec ponado-docker-image --mode typst --no-sha-tag --extra-tag typst-next

# an explicit immutable tag (overrides the computed version tag)
pnpm exec ponado-docker-image --mode typst --no-sha-tag --tag v0.2.0-typst-rc.1

# another registry
pnpm exec ponado-docker-image --mode typst --no-sha-tag \
  --image registry.example.com/you/convert-server --no-mirror
```

The image is built and pushed to GHCR (`ghcr.io/ponado-org/convert-server`).

A missing `vips` binary is not fatal at startup: `/version` reports
`"vips":"disabled"` and `/formats` only advertises what the running image can
actually do. The same applies to `CONVERT_VIPS_BIN` pointing at a path that
does not exist.

GHCR auth needs a **classic** personal access token with `write:packages`
(fine-grained tokens are not supported by GitHub Packages). Create one with only
that scope, avoiding the `repo` scope the UI adds by default:

<https://github.com/settings/tokens/new?scopes=write:packages>

Then log in (keep the token in your password manager, not in the repo):

```bash
export CR_PAT=ghp_...
echo "$CR_PAT" | docker login ghcr.io -u <github-user> --password-stdin
```

The `gh` CLI token also works if you refresh it with the scope
(`gh auth refresh -h github.com -s write:packages`), but a dedicated PAT keeps
Docker's stored credential separate from your `gh` session.

## Gotenberg

Office and HTML conversions are delegated to an existing Gotenberg instance.
Leave `CONVERT_GOTENBERG_URL` empty to disable them (the image and typst work
without Gotenberg).

```yaml
environment:
  CONVERT_GOTENBERG_URL: http://gotenberg:3000
```

The two containers must share a docker network. Either attach convert-server to
Gotenberg's network, or attach Gotenberg to the `convert` network:

```bash
docker network connect gotenberg_default convert-server
```

Routes used:

- LibreOffice: `POST /forms/libreoffice/convert`
- Chromium: `POST /forms/chromium/convert/html` (single `index.html`; relative
  assets are not bundled yet)

Gotenberg's own request body limits, timeouts and LibreOffice settings still
apply. `CONVERT_MAX_BODY_MB` should be at least as large as the files you send.

### Gotenberg proxy

When `CONVERT_GOTENBERG_URL` is set, `/gotenberg/*` is a transparent reverse
proxy to it. The mount prefix is stripped and the method, query, headers and
body are forwarded unchanged:

```bash
# calls Gotenberg's POST /forms/pdf/merge
curl -s localhost:3100/gotenberg/forms/pdf/merge \
  -F 'files=@a.pdf' -F 'files=@b.pdf' -o merged.pdf
```

Use it for Gotenberg features that `/convert` does not wrap (merge, PDF/A,
signing, metadata). Prefer `/convert` for image, PDF and Office conversion so
clients stay backend-agnostic. Set `CONVERT_GOTENBERG_PROXY=false` to disable
the route; when Gotenberg is not configured it returns `501`.

## Tests

```bash
go test ./...                       # everything; integration tests skip without vips
go test ./... -run Integration      # only real vips: PNG/WebP/PDF/SVG round-trips
```

The default suite uses a fake vips CLI and an `httptest` Gotenberg, so it needs
no external services. Integration tests skip themselves when `vips` (or a
required loader such as `pdfload`/`svgload`) is not installed. The Docker image
has everything, so the full set runs there.

## Build caching

Rebuilds are cheap even though the service ships three runtimes (Go, typst,
vips):

- the Go stage runs on the **build platform** and cross-compiles for the
  target (`GOARCH=$TARGETARCH`, CGO off), so the arm64 leg of a multi-arch
  build never executes the toolchain under QEMU;
- Go build and module caches are **BuildKit cache mounts**, so a source change
  recompiles only what changed;
- Docker **layer cache** covers base images, Alpine packages, fonts and the
  typst copy.

For CI or several build machines, export the cache to the registry (needs a
`docker-container` builder - the default docker driver cannot export caches):

```bash
docker buildx create --name convert --driver docker-container --use
pnpm exec ponado-docker-image --mode prod \
  --cache-to type=registry,ref=registry.example.com/you/convert-server:buildcache,mode=max \
  --cache-from type=registry,ref=registry.example.com/you/convert-server:buildcache
```

## Layout

```
main.go                  wiring only (config -> typst + convert -> server)
internal/config/         environment parsing
internal/typst/          typst invocation, shadow dirs, timeouts, concurrency
internal/convert/        format detection, vips pipeline, Gotenberg client
internal/server/         HTTP handlers + JSON/multipart request decoding
.github/workflows/       GitHub Actions image builds (tag push + manual)
package.json             version, dockerImage policy, release scripts
```

## API

| Method | Path            | Returns                                              |
| ------ | --------------- | ---------------------------------------------------- |
| GET    | `/health`       | `ok` (plain text)                                    |
| GET    | `/version`      | `{"service","version","go","typst","vips","gotenberg"}` |
| GET    | `/formats`      | accepted inputs, output targets, enabled backends    |
| POST   | `/typst/render` | `application/pdf`, or `application/json` on error    |
| POST   | `/convert`      | converted bytes, or `application/json` on error      |
| ANY    | `/gotenberg/*`  | transparent proxy to `CONVERT_GOTENBERG_URL` (when enabled) |

### `POST /convert`

Multipart upload (the intended path):

```bash
# HEIC photo -> PNG preview
curl -s localhost:3100/convert \
  -F 'file=@IMG_0001.HEIC' -F 'to=png' -o preview.png

# DOCX -> PDF (requires Gotenberg)
curl -s localhost:3100/convert \
  -F 'file=@report.docx' -F 'to=pdf' -o report.pdf

# page 3 of a PDF -> WebP at 120 DPI, max 1600px wide
curl -s localhost:3100/convert \
  -F 'file=@manual.pdf' -F 'to=webp' -F 'page=3' -F 'dpi=120' \
  -F 'maxWidth=1600' -F 'quality=80' -o page.webp

# let the service choose the browser-safe default
curl -s localhost:3100/convert -F 'file=@scan.tif' -o scan.png
```

Fields:

| Field | Default | Meaning |
| ----- | ------- | ------- |
| `file` (or `files`) | required | Uploaded file part. |
| `to` | auto | `pdf`, `png`, `jpg`/`jpeg`, `webp`, or `auto`. |
| `page` | `1` | 1-based page/frame for PDF, TIFF, HEIC and GIF input. |
| `dpi` | `150` | Rasterisation DPI for PDF/SVG input (`36`-`600`). |
| `maxWidth` | none | Downscale bound in pixels; never upscales. |
| `maxHeight` | none | Downscale bound in pixels; never upscales. |
| `quality` | `85` | JPEG/WebP quality (`1`-`100`). |
| `timeoutMs` | `30000` | Conversion timeout (`1000`-`120000`). |

JSON is available for clients that already hold bytes in memory; `data` is
base64 and `filename` supplies the extension:

```bash
curl -s localhost:3100/convert -H 'content-type: application/json' \
  -d '{"filename":"photo.heic","data":"<base64>","to":"png"}' -o photo.png
```

Responses set `content-type` to the real output type and
`content-disposition: inline`, so `<img src>` and `<iframe src>` work without
downloading. `cache-control: no-store` is set because uploads may be private.

Error statuses for `/convert`:

| Status | Meaning |
| ------ | ------- |
| `400` | Invalid JSON, missing file, bad numeric field, unknown `to`. |
| `408` | Queue or conversion timeout. |
| `413` | Request body exceeds `CONVERT_MAX_BODY_MB`. |
| `415` | Input format or target not supported. |
| `422` | The backend could not convert this file (message included). |
| `501` | Conversion needs Gotenberg, but `CONVERT_GOTENBERG_URL` is unset. |
| `502` | Gotenberg failed or was unreachable. |

### `POST /typst/render`

Clean-break rename: the old `/render` route is now `/typst/render`. The request
shape is unchanged.

**JSON** (`Content-Type: application/json`) - files are base64:

```jsonc
{
  "template": "= Hello\n#let data = json(\"/data.json\")", // required
  "mainFilePath": "/main.typ",        // optional, default /main.typ
  "files": { "/data.json": "<base64>" }, // optional shadow files
  "fontPaths": ["/.fonts"],           // optional shadow dirs -> --font-path
  "inputs": { "version": "1.2.0" },   // optional -> sys.inputs.version
  "timeoutMs": 30000                  // optional, 1000-60000 (default 30000)
}
```

**multipart/form-data** - files as binary parts, no base64 overhead:

- text fields: `template`, `mainFilePath`, `timeoutMs`,
  `inputs` (JSON object string), `fontPaths` (JSON array string)
- file parts land flat in the shadow root by filename:
  `curl -F 'files=@data.json'` -> `/data.json`

```bash
# JSON
curl -s localhost:3100/typst/render -H 'content-type: application/json' \
  -d '{"template":"= convert-server\nHello from the shared service."}' -o hello.pdf

# multipart (binary files)
curl -s localhost:3100/typst/render \
  -F 'template== Report
#let d = json("data.json")
#d.title' \
  -F 'files=@data.json' -F 'timeoutMs=5000' -o report.pdf
```

- Every request renders in its own temp dir; `read("data.json")` resolves
  relative to the main file's directory.
- Client paths are clamped to the shadow dir - `../` traversal is rejected.
- Error statuses: `400` invalid JSON/template, `413` body too large,
  `408` compile/queue timeout, `422` typst compile error (message included).

## Backend tooling

The full image installs only the pieces vips needs; there is no ImageMagick CLI
or poppler-utils dependency:

| Package | Purpose | Build arg |
| ------- | ------- | --------- |
| `vips`, `vips-tools` | CLI, JPEG/PNG/GIF/WebP/TIFF/SVG load+save | `WITH_VIPS` |
| `vips-heif` | HEIC/HEIF/AVIF via libheif (libde265/dav1d) | `WITH_VIPS` |
| `vips-poppler` | PDF input via poppler | `WITH_VIPS` |
| `vips-jxl` | JPEG XL input | `WITH_VIPS_EXTRAS` |
| `vips-magick` | BMP/ICO/PSD/TGA and other ImageMagick formats | `WITH_VIPS_EXTRAS` |

`WITH_VIPS=false` skips the whole graph. `WITH_VIPS_EXTRAS=false` keeps the
core image/PDF formats but drops JPEG XL and the ImageMagick loaders. If you
drop the extras and do not want them advertised, also remove `jxl`, `bmp`,
`ico`, `psd` and `tga` from `internal/convert/format.go`.

Images are transcoded with `vips colourspace` (preceded by `vips thumbnail` when
resizing); PDF/SVG input gets `dpi` and `page` loader options. This normalises
16-bit/CMYK input to 8-bit sRGB, which is what browsers want for previews.
Image -> PDF is handled in Go: vips produces a normalised sRGB JPEG and a small
PDF wrapper embeds it, so no extra binary is needed for that direction.

## Fonts

Fonts live in the container - clients never upload them:

1. **Baked baseline** - Liberation Sans is installed in the image (templates
   that need a common sans fallback rely on it; typst's embedded Libertinus
   alone is not enough).
2. **Shared pool** - every `.ttf`/`.otf` under the mounted font dir
   (`CONVERT_FONTS_DIR`, default `./fonts`) is visible to all renders:

   ```yaml
   volumes:
     - /srv/fonts:/app/fonts:ro
   ```

   New fonts are picked up on the next render, no rebuild needed.

The raw API still accepts per-request files and `fontPaths` for one-off cases,
but the mounted pool is the intended path: it keeps requests small and fonts
shared across apps.

## Configuration

| Env | Default | Meaning |
| --- | ------- | ------- |
| `PORT` | `3100` | Listen port. |
| `CONVERT_TYPST_BIN` | `typst` | Typst binary (path or name on `$PATH`). |
| `CONVERT_VIPS_BIN` | `vips` | libvips CLI (path or name on `$PATH`). Missing binary disables image conversion instead of failing startup. |
| `CONVERT_GOTENBERG_URL` | empty | Gotenberg base URL; enables Office/HTML conversion. |
| `CONVERT_GOTENBERG_PROXY` | `true` | Expose `/gotenberg/*` as a reverse proxy to `CONVERT_GOTENBERG_URL`. |
| `CONVERT_MAX_CONCURRENCY` | NumCPU | Parallel conversions per backend; `<= 0` = unlimited. |
| `CONVERT_MAX_BODY_MB` | `50` | Request body cap in MB. |
| `CONVERT_FONTS_DIR` | `./fonts` | Host font dir mounted at `/app/fonts` (compose). |
| `CONVERT_PORT` | `3100` | Published host port (compose). |
| `CONVERT_IMAGE` | published image | Image ref to pull (compose). |
| `CONVERT_NETWORK` | `convert` | Shared docker network name (compose). |

## Connecting apps

The service joins a fixed-name docker network (`convert`), so another compose
project reaches it without publishing ports:

```yaml
services:
  app:
    environment:
      CONVERT_URL: http://convert-server:3100
    networks: [convert]

networks:
  convert:
    external: true
```

Override the network name with `CONVERT_NETWORK`. Without the shared network,
point clients at the published port (`http://<host>:3100`) instead.

Any HTTP client works: POST to `/convert` with a file part, or to
`/typst/render` with the template contract above.

## Security notes

- Every request gets a fresh temp dir; uploads and outputs are removed when the
  request finishes.
- Client paths are clamped; `../` traversal is rejected.
- Metadata is stripped from image outputs (`keep=none`).
- SVG is rasterised instead of being served inline.
- Untrusted conversions are bounded by `timeoutMs`, `CONVERT_MAX_BODY_MB`, and
  the concurrency cap.
- HTML is rendered by Gotenberg's Chromium; configure Gotenberg's allow/deny
  lists if HTML is untrusted (a page can reference remote resources).
- `/gotenberg/*` exposes Gotenberg's whole HTTP surface. Keep the service on a
  private network, or set `CONVERT_GOTENBERG_PROXY=false`.
- `cache-control: no-store` and `x-content-type-options: nosniff` are set on
  binary responses.

## Migrating from typst-server

This is a clean break:

- image/repo name: `typst-server` -> `convert-server`
- container name: `typst-server` -> `convert-server`
- docker network: `typst` -> `convert`
- `POST /render` -> `POST /typst/render`
- `TYPST_BIN` -> `CONVERT_TYPST_BIN`
- `MAX_CONCURRENCY` -> `CONVERT_MAX_CONCURRENCY`
- `MAX_BODY_MB` -> `CONVERT_MAX_BODY_MB`
- `TYPST_IMAGE` -> `CONVERT_IMAGE`, `TYPST_PORT` -> `CONVERT_PORT`,
  `TYPST_NETWORK` -> `CONVERT_NETWORK`, `TYPST_FONTS_DIR` -> `CONVERT_FONTS_DIR`

The git remote and repository directory still use the old name; rename the
remote/repository separately.

## Versioning & releases

Two independent versions:

- **Service version** - `package.json` `version` is the single source of truth.
  The shared build CLI tags the image `v<version>` and bakes the version into
  the binary and the OCI label, so `/version` reports the exact build. Full
  tags: `vX.Y.Z`, `vX.Y`, `latest`, `sha-<short>`. Variant tags:
  `vX.Y.Z-core`, `core` and `vX.Y.Z-typst`, `typst`.
- **Engine versions** - `TYPST_VERSION` (Dockerfile `ARG`, default `0.15.1`)
  pins the typst CLI; vips and the HEIF/poppler plugins come from Alpine. PDF
  output can change between typst releases, so bump it deliberately:
  `pnpm exec ponado-docker-image --mode prod --build-arg TYPST_VERSION=0.16.0`.

Cut a release with `bumpp` (bumps `package.json`, commits, tags `vX.Y.Z`,
pushes), then the variant images are published first and the full image last:

```bash
pnpm release:patch     # or release:minor / release:major
pnpm release           # choose the bump interactively
```

Both refuse a non-`main` branch and `bumpp --git-check` refuses a dirty tree
before anything is tagged. If a variant fails, `latest` is not moved.

## License

MIT - see [LICENSE](LICENSE).
