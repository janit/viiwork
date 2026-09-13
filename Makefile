.PHONY: build mcp accept test clean docker docker-stable docker-rocm up down

# scripts/version.sh, not `git describe` inline: the private repo carries no
# tags, so describe reports the last one it can still see. See that script.
VERSION ?= $(shell ./scripts/version.sh)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/viiwork ./cmd/viiwork

# Version-stamped like the node: the MCP server reports this string to the
# assistant in its initialize response.
mcp:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/viiwork-mcp ./cmd/viiwork-mcp

# The acceptance checker. Version-stamped like the node because `viiwork-accept
# --version` is what a conversion report records.
accept:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/viiwork-accept ./cmd/viiwork-accept

# TEST_CPUS caps the container fallback. An uncapped compile of the whole tree
# on a host that is also serving live lanes has made a backend miss its health
# check and respawn (gb1, 4 cores, 2026-09-03). Override for a dedicated box.
TEST_CPUS ?= 2

test:
	@if command -v go >/dev/null 2>&1; then \
		go test ./... -v; \
	else \
		echo "go not found on host, running tests in container (--cpus=$(TEST_CPUS))..."; \
		docker run --rm --cpus=$(TEST_CPUS) -v $(CURDIR):/src -w /src -e GOFLAGS=-buildvcs=false golang:1.27.1 go test ./... -v; \
	fi

clean:
	rm -rf bin/

# === Docker builds ===
# One image per engine, under docker/. See BUILDS.md.
#
#   docker-rocm (alias: docker, docker-stable) -> viiwork:latest
#
# One image per engine, all under docker/, named for what they carry rather
# than left implied: Dockerfile.rocm is llama.cpp built for ROCm/gfx906, which
# is the engine the reference fleet runs and the one v2.1.0 ships. The vLLM and
# FreeToken images arrive in v2.2.0 as docker-vllm and docker-freetoken.
#
# VERSION must be passed through: the Dockerfile defaults ARG VERSION to "dev",
# so without this the image reports "dev" from /v1/cluster and /v1/status no
# matter what the tree is tagged — which is worst precisely on a release build,
# where the tag is the whole point.
#
# docker and docker-stable stay as aliases: scripts and habits use them, and
# Radeon VII is the core build, so the unqualified name pointing at it is
# right rather than merely convenient.
docker docker-stable docker-rocm:
	docker build --build-arg VERSION=$(VERSION) -f docker/Dockerfile.rocm -t viiwork .

up:
	docker compose up -d

down:
	docker compose down
