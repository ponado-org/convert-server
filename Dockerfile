# syntax=docker/dockerfile:1
# convert-server: Go handler (stdlib only) + typst CLI + libvips.
#
# Nothing heavy is compiled on the build machine: the Go handler builds in
# seconds, typst is copied from the official image, and conversion tooling
# comes from Alpine packages. TYPST_VERSION is pinned for reproducible builds -
# PDF output can change between typst releases, so bump it deliberately.
ARG TYPST_VERSION=0.15.1
# Service release tag; the shared build CLI passes VERSION from package.json.
ARG VERSION=dev

# The handler cross-compiles for the target (CGO off, pure Go), so foreign
# archs need no QEMU emulation: on an amd64 host an arm64 image compiles
# natively for arm64 instead of running the whole toolchain under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION
WORKDIR /app
COPY go.mod ./
COPY main.go ./
COPY internal ./internal
# Cache mounts: repeated builds reuse the Go build cache (the stdlib compile
# is the expensive part) and the module cache, instead of starting cold every
# time a source file changes.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o convert-server .

FROM ghcr.io/typst/typst:${TYPST_VERSION} AS typst

FROM alpine:3.24
ARG VERSION
# Image variants (build args):
#   WITH_VIPS=true          core image/PDF rasterising (vips, HEIC, poppler)
#   WITH_VIPS_EXTRAS=true   optional ImageMagick/JPEG-XL loaders
#   WITH_VIPS=false         typst-only image: no vips dependency graph
ARG WITH_VIPS=true
ARG WITH_VIPS_EXTRAS=true
LABEL org.opencontainers.image.title="convert-server" \
    org.opencontainers.image.description="Self-hosted file conversion service (Typst, images, Office)" \
    org.opencontainers.image.version="${VERSION}" \
    org.opencontainers.image.source="https://github.com/adopon/convert-server" \
    org.opencontainers.image.licenses="MIT"
# Font discovery: XDG_DATA_HOME=/app makes typst scan /app/fonts, which is the
# mount point for the shared font pool. HOME=/tmp gives the non-root user a
# writable cache dir if a template ever pulls a package.
ENV XDG_DATA_HOME=/app \
    HOME=/tmp
# Conversion stack:
#   vips + vips-tools    - image/PDF/SVG rasterisation and transcoding
#   vips-heif            - HEIC/HEIF/AVIF (libheif with libde265/dav1d)
#   vips-poppler         - PDF input -> raster
#   vips-jxl             - JPEG XL input
#   vips-magick          - BMP/ICO/PSD/TGA and other ImageMagick formats
# Gotenberg is not bundled: point CONVERT_GOTENBERG_URL at the shared instance.
# Liberation Sans is the baseline for invoice/certificate templates; the
# typst-embedded set only carries the fallback (Libertinus).
RUN set -eux; \
    pkgs="ca-certificates font-liberation"; \
    if [ "$WITH_VIPS" = "true" ]; then \
        pkgs="$pkgs vips vips-tools vips-heif vips-poppler"; \
        if [ "$WITH_VIPS_EXTRAS" = "true" ]; then \
            pkgs="$pkgs vips-jxl vips-magick"; \
        fi; \
    fi; \
    apk add --no-cache $pkgs; \
    mkdir -p /app/fonts; \
    chmod 1777 /tmp
COPY --from=typst /bin/typst /usr/local/bin/typst
COPY --from=builder /app/convert-server /usr/local/bin/convert-server
USER 65534:65534
EXPOSE 3100
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:3100/health || exit 1
ENTRYPOINT ["/usr/local/bin/convert-server"]
