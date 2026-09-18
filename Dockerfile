# Build the manager and dns-mcp binaries.
#
# ONE image carries both. They are released together and share the condition
# vocabulary: internal/agent's catalog classifies every reason the operator's
# controllers publish. Each Deployment picks its binary with `command`; see
# config/manager and config/components/dns-mcp.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG GIT_TREE_STATE=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w \
      -X main.version=${VERSION} \
      -X main.gitCommit=${GIT_COMMIT} \
      -X main.gitTreeState=${GIT_TREE_STATE} \
      -X main.buildDate=${BUILD_DATE}" \
    -o manager cmd/main.go

# dns-mcp carries its version as an MCP protocol constant, not an ldflags
# variable, so the build metadata above is not stamped into it.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w" \
    -o dns-mcp ./cmd/dns-mcp

# Use distroless as minimal base image to package the binaries
# Refer to https://github.com/GoogleContainerTools/distroless for more details
#
# The nonroot variant has no shell and no package manager, which matters most
# for dns-mcp: it is the process an untrusted model's tool calls reach. It
# holds no credential of its own — an unbound ServiceAccount, a
# credential-free kubeconfig — see config/components/dns-mcp/service_account.yaml.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/dns-mcp .
USER 65532:65532

# The manager keeps the entrypoint it has always had, so nothing that runs
# this image bare changes behavior. dns-mcp's Deployment overrides it with
# `command`.
ENTRYPOINT ["/manager"]
