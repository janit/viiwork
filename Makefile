.PHONY: build mcp test clean docker docker-stable docker-gfx906 docker-experimental up down

# scripts/version.sh, not `git describe` inline: the private repo carries no
# tags, so describe reports the last one it can still see. See that script.
VERSION ?= $(shell ./scripts/version.sh)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/viiwork ./cmd/viiwork

mcp:
	go build -o bin/viiwork-mcp ./cmd/viiwork-mcp

# TEST_CPUS caps the container fallback. An uncapped compile of the whole tree
# on a host that is also serving live lanes has made a backend miss its health
# check and respawn (gb1, 4 cores, 2026-09-03). Override for a dedicated box.
TEST_CPUS ?= 2

test:
	@if command -v go >/dev/null 2>&1; then \
		go test ./... -v; \
	else \
		echo "go not found on host, running tests in container (--cpus=$(TEST_CPUS))..."; \
		docker run --rm --cpus=$(TEST_CPUS) -v $(CURDIR):/src -w /src -e GOFLAGS=-buildvcs=false golang:1.27.0 go test ./... -v; \
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
# VERSION must be passed through: the Dockerfile defaults ARG VERSION to "dev",
# so without this the image reports "dev" from /v1/cluster and /v1/status no
# matter what the tree is tagged — which is worst precisely on a release build,
# where the tag is the whole point. scripts/update.sh and the gfx906 target
# already do this; this target was the odd one out.
docker docker-stable:
	docker build --build-arg VERSION=$(VERSION) -t viiwork .

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
