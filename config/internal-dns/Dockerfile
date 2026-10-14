# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/internal-dns ./cmd/internal-dns
COPY cmd/internal-dns-coredns ./cmd/internal-dns-coredns
COPY api ./api
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags netgo,osusergo -ldflags='-s -w' \
    -o /out/internal-dns ./cmd/internal-dns && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/coredns ./cmd/internal-dns-coredns

FROM scratch AS runtime
COPY --from=build /out/internal-dns /internal-dns
USER 65532:65532
ENTRYPOINT ["/internal-dns"]

FROM debian:trixie-slim AS serving
RUN apt-get update && apt-get install --no-install-recommends -y \
    dnsdist pdns-recursor iproute2 procps netcat-openbsd ca-certificates bash && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/internal-dns /internal-dns
COPY --from=build /out/coredns /usr/local/bin/coredns
COPY config/internal-dns/commands/ /usr/local/sbin/
USER 65532:65532
RUN for tool in bash ip nc coredns dnsdist pdns_recursor; do \
      command -v "$tool" >/dev/null || exit 1; \
    done && test -x /internal-dns && coredns -version
RUN for helper in /usr/local/sbin/internal-dns-*; do \
      test -x "$helper" && bash -n "$helper" || exit 1; \
    done
ENTRYPOINT ["/internal-dns"]
