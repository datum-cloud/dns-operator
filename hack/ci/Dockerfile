# syntax=docker/dockerfile:1.7
# Both CI images share one compilation stage and the same Go caches.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    export CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH; \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd && \
    go build -trimpath -ldflags="-s -w" -o /out/dns-mcp ./cmd/dns-mcp && \
    go build -trimpath -o /out/internal-dns ./cmd/internal-dns && \
    go build -trimpath -o /out/coredns ./cmd/internal-dns-coredns && \
    go build -trimpath -o /out/dns-qualify ./test/internaldns/kubernetes/qualify

FROM gcr.io/distroless/static:nonroot AS public
COPY --from=build /out/manager /manager
COPY --from=build /out/dns-mcp /dns-mcp
USER 65532:65532
ENTRYPOINT ["/manager"]

FROM powerdns/dnsdist-20:2.0.10@sha256:24ac720af997aff6b98cb5b50d748754f815428e07b5368dfb0eccd5aa8d07e8 AS private
USER 0:0
RUN apt-get update && apt-get install --no-install-recommends -y \
    pdns-recursor iproute2 iptables procps netcat-openbsd ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/internal-dns /internal-dns
COPY --from=build /out/dns-qualify /dns-qualify
COPY --from=build /out/coredns /usr/local/bin/coredns
COPY config/internal-dns/commands/ /usr/local/sbin/
USER 65532:65532
RUN coredns -version
ENTRYPOINT ["/internal-dns"]
