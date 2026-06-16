# syntax=docker/dockerfile:1.7

# Build stage: produce a fully static binary so the runtime image needs no
# libc, no shell, no package manager — just our binary and CA certs.
FROM --platform=$BUILDPLATFORM golang:1.26.1-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src

# Module cache: copy go.{mod,sum} first so dependency resolution is reused
# across source-only rebuilds.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

# CGO_ENABLED=0 with the static linker keeps the binary self-contained for
# distroless/static. -trimpath strips local file paths; -X embeds the version.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/localfront \
      ./cmd/localfront

# Runtime stage: distroless static carries CA bundles, /etc/passwd with a
# nonroot user, and tzdata — and nothing else. No shell, no apt, no busybox.
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/mackee/localfront"
LABEL org.opencontainers.image.description="A local Amazon CloudFront emulator driven by CloudFormation templates."
LABEL org.opencontainers.image.licenses="MIT"

# /tmp is writable for QuickJS function sandboxes (each compiled function
# allocates a tempdir under os.TempDir()). The Compose / k8s examples mount
# tmpfs here; this VOLUME makes the same available out of the box.
VOLUME ["/tmp"]

COPY --from=build /out/localfront /usr/local/bin/localfront

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/localfront"]
CMD ["serve"]
