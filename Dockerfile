# syntax=docker/dockerfile:1

# Build stages run on the build platform and cross-compile for the target.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
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
    REPO_CACHE_DIR=/data/repos
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/agent-nexus"]
CMD ["serve"]
