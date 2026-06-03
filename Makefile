# Convenience targets. CI/Docker uses `go build` directly.

BIN := lightxrayd
PKG := ./cmd/lightxrayd

.PHONY: build run tidy fmt vet test clean docker

build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BIN) $(PKG)

run:
	@test -f .env || (echo "create .env first (cp .env.example .env)"; exit 1)
	set -a; . ./.env; set +a; go run $(PKG)

tidy:
	go mod tidy

fmt:
	gofmt -s -w .

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -f $(BIN)

# Build a linux/amd64 static binary without needing Go locally.
docker:
	docker build -f deploy/Dockerfile -t lightxray:dev .
	docker run --rm lightxray:dev cat /usr/local/bin/lightxrayd > $(BIN)
	chmod +x $(BIN)
