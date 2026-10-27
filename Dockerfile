# One image holds every Strait binary and the man pages.
FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/takodotid/strait/internal/version.Version=${VERSION}" -o /out/ ./cmd/... \
 && go run ./hack/gendocs /out/man/man1

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/strait /out/strait-controller /out/strait-gateway /usr/local/bin/
COPY --from=build /out/man/man1 /usr/share/man/man1
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/strait"]
