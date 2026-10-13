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
    bind9 bind9-utils dnsdist iproute2 procps netcat-openbsd ca-certificates bash jq && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/internal-dns /internal-dns
COPY config/internal-dns/commands/ /usr/local/sbin/
USER 65532:65532
RUN for tool in bash jq date ip nc named named-checkconf named-checkzone dnsdist rndc; do \
      command -v "$tool" >/dev/null || exit 1; \
    done && jq --version && test -x /internal-dns
RUN for helper in /usr/local/sbin/internal-dns-*; do \
      test -x "$helper" && bash -n "$helper" || exit 1; \
    done
ENTRYPOINT ["/internal-dns"]
