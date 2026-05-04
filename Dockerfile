# ── Stage 1: Build ───────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o bin/shirakami ./cmd/analyze/ && \
    go build -o bin/shirakami-server ./cmd/server/

# ── Stage 2: gopls (Go LSP) ──────────────────────────────────────────────────
FROM golang:1.25-alpine AS gopls-builder
RUN go install golang.org/x/tools/gopls@latest

# ── Stage 3: pyright (Python LSP) ────────────────────────────────────────────
# pyright is the only Python LSP with full callHierarchy support (incomingCalls)
# pylsp and jedi-language-server do NOT support callHierarchy
FROM node:20-alpine AS pyright-builder
RUN npm install -g pyright

# ── Stage 4: Runtime ─────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache \
    git \
    ca-certificates \
    tzdata \
    openssh-client \
    nodejs \
    npm

# ripgrep
RUN apk add --no-cache ripgrep || \
    (apk add --no-cache --repository=https://dl-cdn.alpinelinux.org/alpine/edge/community ripgrep)

# gopls — for Go repos
COPY --from=gopls-builder /go/bin/gopls /usr/local/bin/gopls

# pyright — for Python repos (callHierarchy support)
COPY --from=pyright-builder /usr/local/lib/node_modules/pyright /usr/local/lib/node_modules/pyright
# Use a shell wrapper instead of a symlink to ensure node is invoked correctly
RUN printf '#!/bin/sh\nexec node /usr/local/lib/node_modules/pyright/dist/pyright-langserver.js "$@"\n' \
    > /usr/local/bin/pyright-langserver && chmod +x /usr/local/bin/pyright-langserver

WORKDIR /app

COPY --from=builder /src/bin/shirakami     ./bin/shirakami
COPY --from=builder /src/bin/shirakami-server ./bin/shirakami-server
COPY migrations/ ./migrations/

ENV TZ=Asia/Shanghai

CMD ["./bin/shirakami"]
