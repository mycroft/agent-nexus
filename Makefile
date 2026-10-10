VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= registry.mkz.me/mycroft/agent-nexus
TAG ?= dev
# The Dockerfile needs BuildKit: docker with buildx, or podman.
CONTAINER_TOOL ?= docker

BIN := $(CURDIR)/bin

.PHONY: build run test vet lint docker-build clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN)/agent-nexus ./cmd/agent-nexus

# Reads its configuration from the environment. Pass flags with ARGS="...".
run:
	go run ./cmd/agent-nexus serve $(ARGS)

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run -c .golangci.yaml ./...

docker-build:
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .

clean:
	rm -rf $(BIN)
