#
# CertsForever container image.
#
#   docker build -t certsforever .
#   docker build --build-arg VERSION=v0.1.0 --build-arg COMMIT=$(git rev-parse --short HEAD) -t certsforever .
#
# The base images are build args so they can be pinned or mirrored
# (e.g. behind a registry proxy) without editing this file.
ARG GO_IMAGE=golang:1.26-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/base-debian12:nonroot

# ---- build -----------------------------------------------------------------
FROM ${GO_IMAGE} AS build
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
ARG COMMIT=
ENV GOPROXY=${GOPROXY} \
    CGO_ENABLED=1 \
    GOFLAGS=-mod=readonly
WORKDIR /src

# Dependencies first, so source edits don't invalidate the module layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath \
      -ldflags="-s -w -X certsforever/internal/buildinfo.Version=${VERSION} -X certsforever/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/certsforever ./cmd/certsforever \
 && mkdir -p /out/data/backups

# ---- runtime ---------------------------------------------------------------
# distroless/base has glibc (needed by the cgo SQLite driver), CA certs and
# tzdata, and no shell or package manager.
FROM ${RUNTIME_IMAGE}
COPY --from=build /out/certsforever /usr/local/bin/certsforever
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
WORKDIR /data
ENV CERTS_ENV=production \
    CERTS_ADDR=:8080 \
    CERTS_DB=/data/certs.db
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/certsforever", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/certsforever"]
CMD ["serve"]
