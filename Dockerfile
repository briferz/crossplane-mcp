# syntax=docker/dockerfile:1

# Cross-compiles the static binary for the target platform, then ships it in a
# minimal distroless image. CGO is off (pure-Go client-go), so this is a fully
# static binary; distroless/static provides CA certificates for talking to the
# Kubernetes API over TLS.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/crossplane-mcp ./cmd/crossplane-mcp

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/crossplane-mcp /usr/local/bin/crossplane-mcp
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/crossplane-mcp"]
