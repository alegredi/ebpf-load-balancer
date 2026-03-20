CLANG ?= clang
CFLAGS ?= -O2 -g -Wall -Werror

.PHONY: all generate build test clean

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

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf bin
