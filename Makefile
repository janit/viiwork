.PHONY: build mcp test clean docker docker-stable docker-gfx906 docker-experimental up down

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/viiwork ./cmd/viiwork

mcp:
	go build -o bin/viiwork-mcp ./cmd/viiwork-mcp

test:
	@if command -v go >/dev/null 2>&1; then \
		go test ./... -v; \
	else \
		echo "go not found on host, running tests in container..."; \
		docker run --rm -v $(CURDIR):/src -w /src golang:1.23.6 go test ./... -v; \
	fi

clean:
	rm -rf bin/

# === Docker builds ===
# viiwork ships in two parallel images that share the Go server but
# differ in the llama.cpp binary they spawn. See BUILDS.md for the
# full comparison and rollout guidance.
#
#   docker / docker-stable           -> viiwork:latest  (upstream llama.cpp)
#   docker-gfx906 / docker-experimental -> viiwork:gfx906 (stripped fork)
#
# The two pairs are aliases so the Makefile reads symmetrically with
# the language used in BUILDS.md and scripts/setup-node.sh, while
# keeping the original target names working for older docs and habits.

# Stable foundation: standard upstream llama.cpp from the default Dockerfile.
docker docker-stable:
	docker build -t viiwork .

# Experimental track: gfx906-stripped fork build. Requires the local fork
# tree at $(GFX906_FORK) and uses BuildKit's --build-context to pull it
# into the build without bloating the main viiwork build context.
GFX906_FORK ?= $(HOME)/gfx906-work/llama.cpp-gfx906
docker-gfx906 docker-experimental:
	@test -d "$(GFX906_FORK)/.git" || (echo "fork tree not found at $(GFX906_FORK)" >&2; exit 2)
	DOCKER_BUILDKIT=1 docker build \
	    -t viiwork:gfx906 \
	    -f Dockerfile.gfx906 \
	    --build-context fork=$(GFX906_FORK) \
	    --build-arg VERSION=$(VERSION)-gfx906 \
	    .

up:
	docker compose up -d

down:
	docker compose down
