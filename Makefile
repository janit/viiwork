.PHONY: build build-darwin mcp install-mcp accept release test vet-cross clean docker docker-stable docker-rocm docker-vllm docker-freetoken docker-strata docker-strata-cuda up down

# scripts/version.sh, not `git describe` inline: the private repo carries no
# tags, so describe reports the last one it can still see. See that script.
VERSION ?= $(shell ./scripts/version.sh)

build:
	./scripts/gobuild.sh ./cmd/viiwork bin/viiwork $(VERSION)

# A native macOS (Apple Silicon) node and its acceptance checker, built from any
# host. No cgo, so no Xcode toolchain is needed to cross-compile. docs/macos.md.
build-darwin:
	GOOS=darwin GOARCH=arm64 ./scripts/gobuild.sh ./cmd/viiwork bin/darwin-arm64/viiwork $(VERSION)
	GOOS=darwin GOARCH=arm64 ./scripts/gobuild.sh ./cmd/viiwork-accept bin/darwin-arm64/viiwork-accept $(VERSION)

# Version-stamped like the node: the MCP server reports this string to the
# assistant in its initialize response.
mcp:
	./scripts/gobuild.sh ./cmd/viiwork-mcp bin/viiwork-mcp $(VERSION)

# Onto PATH, so an MCP client config can name the binary rather than a checkout.
PREFIX ?= $(HOME)/.local
install-mcp: mcp
	install -D -m 0755 bin/viiwork-mcp $(PREFIX)/bin/viiwork-mcp

# The acceptance checker. Version-stamped like the node because `viiwork-accept
# --version` is what a conversion report records.
accept:
	./scripts/gobuild.sh ./cmd/viiwork-accept bin/viiwork-accept $(VERSION)

# Every release target and its archives, as the release workflow builds them:
# make release VERSION=v2.6.0 (docs/releases.md).
release:
	./scripts/build-release.sh $(VERSION) dist

# TEST_CPUS caps the container fallback. An uncapped compile of the whole tree
# on a host that is also serving live lanes has made a backend miss its health
# check and respawn (gb1, 4 cores, 2026-09-03). Override for a dedicated box.
TEST_CPUS ?= 2

test:
	@if command -v go >/dev/null 2>&1; then \
		go test ./... -v && $(MAKE) --no-print-directory vet-cross; \
	else \
		echo "go not found on host, running tests in container (--cpus=$(TEST_CPUS))..."; \
		docker run --rm --cpus=$(TEST_CPUS) -v $(CURDIR):/src -w /src -e GOFLAGS=-buildvcs=false golang:1.27.2 \
			sh -c 'go test ./... -v && GOOS=linux GOARCH=amd64 go vet ./... && GOOS=darwin GOARCH=arm64 go vet ./...'; \
	fi

# The tree builds for Linux and macOS from either one. Tests run only on the
# host's own OS, so vet both: a /proc read slipping into a darwin build, or a
# darwin-only call into a Linux one, fails here rather than on the machine.
vet-cross:
	GOOS=linux GOARCH=amd64 go vet ./...
	GOOS=darwin GOARCH=arm64 go vet ./...

clean:
	rm -rf bin/

# === Docker builds ===
# One image per engine, under docker/. See BUILDS.md.
#
#   docker-rocm (alias: docker, docker-stable) -> viiwork:latest
#   docker-vllm                                 -> viiwork-vllm:latest
#   docker-freetoken                            -> viiwork-freetoken:latest
#   docker-strata                               -> viiwork-strata:latest
#   docker-strata-cuda                          -> viiwork-strata-cuda:latest
#
# One image per engine, all under docker/, named for the ENGINE rather than for
# a GPU vendor: Dockerfile.rocm is llama.cpp built for ROCm/gfx906, which is the
# engine the reference fleet runs. vLLM already has a ROCm build and FreeToken
# may add one, so a second accelerator backend is a sibling base image, never a
# fork.
#
# The two v2.2.0 images are not peers in cost. docker-vllm drops the binary into
# vLLM's published image and takes about a minute; docker-freetoken installs the
# engine, torch and its CUDA wheels into a devel CUDA base and is several GB.
# Build that one off-peak.
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

docker-vllm:
	docker build --build-arg VERSION=$(VERSION) -f docker/Dockerfile.vllm -t viiwork-vllm .

docker-freetoken:
	docker build --build-arg VERSION=$(VERSION) -f docker/Dockerfile.freetoken -t viiwork-freetoken .

docker-strata:
	docker build --build-arg VERSION=$(VERSION) -f docker/Dockerfile.strata -t viiwork-strata .

# The CUDA image the next target sits on (STRATA_CUDA_IMAGE): upstream's
# tooling and signed engine package at the pin. Needs the internet.
docker-strata-cuda-base:
	scripts/build-strata-cuda-base.sh

# On that image (STRATA_CUDA_IMAGE); run docker-strata-cuda-base first.
docker-strata-cuda:
	docker build --build-arg VERSION=$(VERSION) -f docker/Dockerfile.strata-cuda -t viiwork-strata-cuda .

up:
	docker compose up -d

down:
	docker compose down
