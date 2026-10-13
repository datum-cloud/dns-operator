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

FROM scratch AS runtime
COPY --from=build /out/internal-dns /internal-dns
USER 65532:65532
ENTRYPOINT ["/internal-dns"]

FROM debian:trixie-slim AS serving
RUN apt-get update && apt-get install --no-install-recommends -y \
    bind9 bind9-utils dnsdist iproute2 procps netcat-openbsd ca-certificates bash && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/internal-dns /internal-dns
COPY config/internal-dns/commands/ /usr/local/sbin/
USER 65532:65532
ENTRYPOINT ["/internal-dns"]
