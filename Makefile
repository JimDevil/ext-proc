VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.1.0)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE     ?= ghcr.io/jimdevil/ext-proc
PLATFORMS ?= linux/amd64,linux/arm64

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(BUILD_DATE)

.PHONY: build build-all vet test test-repeat fmt fmt-check lint coverage ci vendor run clean image image-multi image-smoke image-clean e2e loadtest

build:
	mkdir -p bin
	go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/ext-proc .

build-all:
	go build ./...

vet:
	go vet ./...

test:
	go test -race -shuffle=on -count=1 ./...

test-repeat:
	go test -race -shuffle=on -count=2 ./...

fmt:
	gofmt -w $$(find . -type f -name "*.go" -not -path "./vendor/*")

fmt-check:
	@test -z "$$(gofmt -l $$(find . -type f -name '*.go' -not -path './vendor/*'))" || \
		(echo "gofmt required for:"; gofmt -l $$(find . -type f -name '*.go' -not -path './vendor/*'); exit 1)

lint: build-all vet fmt-check

coverage:
	go test -race -shuffle=on -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

ci: build-all vet fmt-check test-repeat

vendor:
	go mod tidy && go mod vendor

run: build
	./bin/ext-proc

# Build a single-architecture image for the local platform (docker build).
image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

# Build & push multi-arch images (amd64 + arm64). Requires buildx and a
# logged-in registry (--push is mandatory for multi-platform manifests;
# a local-only variant would need a containerd image store).
image-multi:
	docker buildx build --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest \
		--push .

# Smoke-run the local image: container starts, metrics and readiness answer.
image-smoke: image
	docker run -d --rm --name ext-proc-smoke -p 19090:19090 $(IMAGE):$(VERSION)
	@sleep 2
	curl -sf http://localhost:19090/metrics -o /dev/null && echo "metrics OK" || (echo "metrics FAILED"; docker logs ext-proc-smoke; exit 1)
	curl -sf http://localhost:19090/readyz -o /dev/null && echo "readyz OK" || (echo "readyz FAILED"; docker logs ext-proc-smoke; exit 1)
	docker stop ext-proc-smoke

image-clean:
	docker rmi $(IMAGE):$(VERSION) $(IMAGE):latest 2>/dev/null || true

# Local Envoy -> ext-proc -> echo stack (requires Docker + compose).
e2e:
	./test/e2e/run.sh

# Load test against the running e2e stack (requires k6).
loadtest:
	k6 run test/e2e/loadtest.js

clean:
	rm -rf bin/ext-proc coverage.out
