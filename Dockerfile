# syntax=docker/dockerfile:1

# Cross-compilation: the build stage always runs on the builder's own
# architecture and Go compiles for the target, so building linux/arm64 on an
# amd64 runner (or the reverse) needs no emulation.
# Keep the Go version in sync with the `go` directive in go.mod.
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api"]
