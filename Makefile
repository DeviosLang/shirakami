BINARY      = bin/shirakami
SERVER_BIN  = bin/shirakami-server
VERSION     = 0.1.0
ANALYZE_PKG = github.com/DeviosLang/shirakami/cmd/analyze
SERVER_PKG  = github.com/DeviosLang/shirakami/cmd/server

.PHONY: build build-server test lint migrate run run-server clean benchmark

build:
	mkdir -p bin
	go build -ldflags="-X main.version=$(VERSION)" -o $(BINARY) $(ANALYZE_PKG)

build-server:
	mkdir -p bin
	go build -ldflags="-X main.version=$(VERSION)" -o $(SERVER_BIN) $(SERVER_PKG)

build-all: build build-server

test:
	go test ./...

test-integration:
	go test ./tests/e2e/... -v -count=1 -timeout=5m

benchmark: build
	./$(BINARY) benchmark run --golden-dir tests/golden/cases --fail-below 0.90

lint:
	go vet ./...

migrate:
	@echo "Use 'goose -dir migrations postgres <DSN> up' to run migrations."

run: build
	./$(BINARY)

run-server: build-server
	./$(SERVER_BIN)

clean:
	rm -rf bin/
