# syntax=docker/dockerfile:1

# Cross-compiles the static binary for the target platform, then ships it in a
# minimal distroless image. CGO is off (pure-Go client-go), so this is a fully
# static binary; distroless/static-debian13 provides CA certificates for talking
# to the Kubernetes API over TLS.
#
# The base is pinned to the explicit -debian13 variant rather than the bare
# `static` alias: upstream's build config only produces the debianNN images, and
# the two tags resolve to the same digest today, so this names what we actually
# get instead of relying on an undocumented alias. Trade-off: this no longer
# auto-follows distroless to the next Debian base, so bump it when debian13
# nears EOL or the CA-certificate store stops being refreshed.
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

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/crossplane-mcp /usr/local/bin/crossplane-mcp
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/crossplane-mcp"]
