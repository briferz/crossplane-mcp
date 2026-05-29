BINARY := bin/crossplane-mcp
PKG := ./cmd/crossplane-mcp
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

IMAGE ?= ghcr.io/briferz/crossplane-mcp

.PHONY: build test vet fmt fmt-check lint vulncheck check docker clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) $(PKG)

test:
	go test -race -covermode=atomic -coverprofile=coverage.out ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

lint:
	golangci-lint run

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Mirror the CI gates locally.
check: fmt-check vet lint test vulncheck

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

clean:
	rm -rf bin coverage.out
