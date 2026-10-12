# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY api ./api
COPY internal ./internal
COPY cmd/internal-dns ./cmd/internal-dns
COPY test/internaldns/kubernetes/qualify ./test/internaldns/kubernetes/qualify
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/internal-dns ./cmd/internal-dns && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/dns-qualify ./test/internaldns/kubernetes/qualify

# Debian trixie supplies BIND 9.20 with PROXYv2 and dnsdist. The qualification
# records the resolved image ID and daemon versions rather than relying on tags.
FROM debian:trixie-slim
RUN apt-get update && apt-get install --no-install-recommends -y \
    bind9 bind9-utils dnsdist iproute2 iptables procps netcat-openbsd ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/internal-dns /internal-dns
COPY --from=build /out/dns-qualify /dns-qualify
COPY test/internaldns/kubernetes/commands/ /usr/local/sbin/
USER 65532:65532
ENTRYPOINT ["/internal-dns"]
