ARG ALPINE_IMAGE=alpine:3@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG GO_IMAGE=golang:1.27-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd

# Cross-compile the static binary on the native build platform.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/ludock ./cmd/ludock

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
