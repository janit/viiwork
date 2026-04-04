.PHONY: build mcp test clean docker up down

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/viiwork ./cmd/viiwork

mcp:
	go build -o bin/viiwork-mcp ./cmd/viiwork-mcp

test:
	go test ./... -v

clean:
	rm -rf bin/

docker:
	docker build -t viiwork .

up:
	docker compose up -d

down:
	docker compose down
