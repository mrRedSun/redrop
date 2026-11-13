# syntax=docker/dockerfile:1.7
# Multi-arch image: build stages run on the build platform and cross-compile.

ARG GO_VERSION=1.26
ARG NODE_VERSION=24

# --- Dashboard (embedded into the server binary) ---
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
COPY internal/webui/dist/.gitkeep /src/internal/webui/dist/.gitkeep
RUN npm run build

# --- Go module cache shared by the CLI and server stages ---
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS gobase
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

# --- CLI binaries for every supported platform (served by /install.sh) ---
FROM gobase AS cli
ARG VERSION=dev
RUN set -eu; mkdir -p /out/cli; \
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
      os=${target%/*}; arch=${target#*/}; ext=""; [ "$os" = windows ] && ext=.exe; \
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o "/out/cli/redrop-$os-$arch$ext" ./cmd/redrop; \
    done; \
    cd /out/cli && sha256sum redrop-* > SHA256SUMS

# --- Server for the target platform ---
FROM gobase AS server
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/mrRedSun/redrop/internal/httpapi.Version=${VERSION}" \
      -o /out/redrop-server ./cmd/redrop-server \
 && mkdir -p /out/data

# --- Runtime: distroless, non-root ---
FROM gcr.io/distroless/static-debian13:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="redrop" \
      org.opencontainers.image.description="Self-hosted sharing for agent artifacts" \
      org.opencontainers.image.source="https://github.com/mrRedSun/redrop" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=server /out/redrop-server /usr/local/bin/redrop-server
COPY --from=cli /out/cli /usr/share/redrop/cli
COPY --from=server --chown=65532:65532 /out/data /data
ENV REDROP_LISTEN=:8080 \
    REDROP_DATA_DIR=/data \
    REDROP_CLI_DIR=/usr/share/redrop/cli
EXPOSE 8080
VOLUME ["/data"]
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/redrop-server", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/redrop-server"]
CMD ["serve"]
