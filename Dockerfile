ARG ALPINE_IMAGE=alpine:3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
ARG GO_IMAGE=golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb

# Cross-compile the static binary on the native build platform. Modules
# download into a cache mount, which stays out of the layers: fetching them
# again is faster than restoring them from a layer cache.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS go
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOOS=$TARGETOS GOARCH=$TARGETARCH
COPY go.mod go.sum ./

# The third-party packages the binary imports.
FROM go AS imports
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
  go list -deps -f '{{if and .Module (not .Module.Main)}}{{.ImportPath}}{{end}}' ./cmd/ludock > /imports

# Compiling them and the standard library in their own layer keeps that work
# cached until the list changes, so a code change recompiles only Ludock.
FROM go AS build
COPY --from=imports /imports /imports
RUN --mount=type=cache,target=/go/pkg/mod go build $(cat /imports)
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
  go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/ludock ./cmd/ludock

FROM ${ALPINE_IMAGE} AS runtime
# Helper containers reuse this image and run `ludock helper` directly.
RUN apk upgrade --no-cache
COPY --from=build /out/ludock /usr/local/bin/ludock

ENV LUDOCK_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/ludock", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/ludock"]
CMD ["serve"]
