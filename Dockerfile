# syntax=docker/dockerfile:1
# One image holds every Strait binary and the man pages.
# The build stage runs on the builder's platform and cross-compiles, so a multi-arch build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:9048d5e7751706200eb1719c7dae0436950d362f29a1da91fa05e5c63867c60f AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    go run ./hack/gendocs /out/man/man1 \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X github.com/takodotid/strait/internal/version.Version=${VERSION}" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
LABEL org.opencontainers.image.source=https://github.com/takodotid/strait \
      org.opencontainers.image.licenses=Apache-2.0
COPY --from=build /out/strait /out/strait-controller /out/strait-gateway /usr/local/bin/
COPY --from=build /out/man/man1 /usr/share/man/man1
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/strait"]
