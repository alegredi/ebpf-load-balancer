CLANG ?= clang
CFLAGS ?= -O2 -g -Wall -Werror

.PHONY: all generate build test bench test-netns clean

all: generate build

generate:
	@echo "==> Generating eBPF bindings with bpf2go..."
	go generate ./...

build:
	@echo "==> Building proxy binary..."
	@mkdir -p bin
	go build -v -o bin/proxy ./cmd/proxy

test:
	@echo "==> Running tests..."
	go test -v -race ./internal/... ./pkg/...

bench:
	@echo "==> Running benchmarks..."
	go test -bench=. -benchmem ./internal/... ./pkg/...

test-netns: build
	@echo "==> Running netns integration test..."
	sudo ./scripts/test_netns.sh

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf bin
