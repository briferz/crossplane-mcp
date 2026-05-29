BINARY := bin/crossplane-mcp
PKG := ./cmd/crossplane-mcp
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet fmt fmt-check clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

clean:
	rm -rf bin
