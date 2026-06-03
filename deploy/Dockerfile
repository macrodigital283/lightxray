# Multi-stage build: produces a ~25 MB static binary in a distroless runtime.
# Run from repo root:
#   docker build -f deploy/Dockerfile -t lightxray:dev .

FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum* ./
COPY . .
# Generate go.sum on first build; subsequent builds with a committed
# go.sum reuse the module cache. Safe either way.
RUN go mod tidy
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/lightxrayd ./cmd/lightxrayd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lightxrayd /usr/local/bin/lightxrayd
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/lightxrayd"]
