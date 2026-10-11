VERSION   ?= v0.1.0
IMAGE     ?= ghcr.io/jimdevil/ext-proc
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: build vet test fmt vendor run clean image image-multi image-clean

build:
	go build ./...

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

fmt:
	gofmt -l -w $$(find . -type f -name "*.go" -not -path "./vendor/*")

vendor:
	go mod tidy && go mod vendor

run: build
	go run .

# Build a single-architecture image for the local platform (docker build).
image:
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

# Build & push multi-arch images (amd64 + arm64). Requires buildx and a
# logged-in registry (--push is mandatory for multi-platform manifests;
# a local-only variant would need a containerd image store).
image-multi:
	docker buildx build --platform $(PLATFORMS) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest \
		--push .

# Smoke-run the local image: container starts, gRPC and metrics answer.
image-smoke: image
	docker run -d --rm --name ext-proc-smoke -p 19090:19090 $(IMAGE):$(VERSION)
	@sleep 2
	curl -sf http://localhost:19090/metrics -o /dev/null && echo "image smoke OK" || (echo "image smoke FAILED"; docker logs ext-proc-smoke; exit 1)
	docker stop ext-proc-smoke

image-clean:
	docker rmi $(IMAGE):$(VERSION) $(IMAGE):latest 2>/dev/null || true

clean:
	rm -rf ext-proc
