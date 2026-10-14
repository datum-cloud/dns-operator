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
COPY cmd/internal-dns-coredns ./cmd/internal-dns-coredns
COPY test/internaldns/kubernetes/qualify ./test/internaldns/kubernetes/qualify
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/internal-dns ./cmd/internal-dns && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/coredns ./cmd/internal-dns-coredns && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/dns-qualify ./test/internaldns/kubernetes/qualify

FROM debian:trixie-slim
RUN apt-get update && apt-get install --no-install-recommends -y \
    dnsdist pdns-recursor iproute2 iptables procps netcat-openbsd ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/internal-dns /internal-dns
COPY --from=build /out/dns-qualify /dns-qualify
COPY --from=build /out/coredns /usr/local/bin/coredns
COPY config/internal-dns/commands/ /usr/local/sbin/
USER 65532:65532
RUN coredns -version
ENTRYPOINT ["/internal-dns"]
