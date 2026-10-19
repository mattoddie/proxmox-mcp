# syntax=docker/dockerfile:1

# The binary is static Go, so the build stage runs on the build machine's own
# platform and cross-compiles: multi-arch builds don't need emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY docs/tools.md ./docs/tools.md
RUN CGO_ENABLED=0 go test ./...
# Release builds pass the version from the Git tag (e.g. 1.2.3).
ARG VERSION=0.0.0-dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/mattoddie/proxmox-mcp/internal/tools.Version=${VERSION}" \
      -o /out/proxmox-mcp ./cmd/proxmox-mcp

# Distroless: no shell, no package manager, just CA certificates, tzdata and a
# non-root user (65532), so there is very little for CVE scanners to find.
FROM gcr.io/distroless/static-debian13:nonroot
LABEL org.opencontainers.image.title="proxmox-mcp" \
      org.opencontainers.image.description="MCP server for Proxmox VE" \
      org.opencontainers.image.source="https://github.com/mattoddie/proxmox-mcp" \
      org.opencontainers.image.documentation="https://github.com/mattoddie/proxmox-mcp#readme" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/proxmox-mcp /proxmox-mcp
# Inside the container the server must listen on all interfaces for a published port to
# reach it (the binary itself defaults to 127.0.0.1). MCP_AUTH_TOKEN is still required.
ENV MCP_TRANSPORT=http \
    MCP_HTTP_HOST=0.0.0.0 \
    MCP_HTTP_PORT=8080
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/proxmox-mcp", "healthcheck"]
ENTRYPOINT ["/proxmox-mcp"]
