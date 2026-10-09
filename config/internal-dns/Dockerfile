# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/internal-dns ./cmd/internal-dns
COPY api ./api
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags netgo,osusergo -ldflags='-s -w' \
    -o /out/internal-dns ./cmd/internal-dns

# This is the control-plane image and the binary artifact used by serving
# images. Node, regional-frontend, and regional-BIND images must also contain
# the validation, reload, cache-flush, and fail-closed commands declared in
# their serving configuration.
FROM scratch
COPY --from=build /out/internal-dns /internal-dns
USER 65532:65532
ENTRYPOINT ["/internal-dns"]
