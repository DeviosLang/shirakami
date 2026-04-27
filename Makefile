BINARY   = bin/shirakami
VERSION  = 0.1.0
PKG      = github.com/DeviosLang/shirakami/cmd/analyze

.PHONY: build test lint migrate run clean

build:
	mkdir -p bin
	go build -ldflags="-X main.version=$(VERSION)" -o $(BINARY) $(PKG)

test:
	go test ./...

lint:
	go vet ./...

migrate:
	@echo "No migrations defined yet."

run: build
	./$(BINARY)

clean:
	rm -rf bin/
