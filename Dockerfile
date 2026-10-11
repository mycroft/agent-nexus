# syntax=docker/dockerfile:1

# Build stages run on the build platform and cross-compile for the target.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
ARG BUILDARCH
ARG VERSION=dev
ARG TAILWIND_VERSION=v4.3.3

RUN case "$BUILDARCH" in \
      amd64) arch=x64 ;; \
      arm64) arch=arm64 ;; \
      *) echo "unsupported build architecture: $BUILDARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL -o /usr/local/bin/tailwindcss \
      "https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-linux-${arch}" && \
    chmod +x /usr/local/bin/tailwindcss

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN tailwindcss -i internal/web/styles/app.css -o internal/web/static/css/app.css --minify
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/agent-nexus ./cmd/agent-nexus

# Not distroless: reviews read repositories through the git CLI.
FROM docker.io/library/alpine:3.24
RUN apk add --no-cache git ca-certificates \
 && mkdir /data \
 && chown 65532:65532 /data
COPY --from=build /out/agent-nexus /usr/local/bin/agent-nexus
USER 65532:65532
ENV DB_PATH=/data/agent-nexus.db \
    REPO_CACHE_DIR=/data/repos \
    CHART_CACHE_DIR=/data/charts
EXPOSE 8080 8081 9090
ENTRYPOINT ["/usr/local/bin/agent-nexus"]
CMD ["serve"]
