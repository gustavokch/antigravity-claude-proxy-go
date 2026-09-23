BINARY_NAME=antigravity-proxy
BIN_DIR=bin
VERSION?=1.0.0
LDFLAGS=-s -w -X main.version=$(VERSION)

.PHONY: all build test clean cross-compile dist install install-hooks fmt fmt-check

all: build

build:
	@mkdir -p $(BIN_DIR)
	@echo "Building $(BINARY_NAME) for current platform..."
	go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/proxy

test:
	@echo "Running tests..."
	go test -v -race ./...

clean:
	@echo "Cleaning up..."
	@rm -rf $(BIN_DIR) proxy

cross-compile: dist

dist:
	@mkdir -p $(BIN_DIR)
	@echo "Building cross-platform release binaries..."

	@echo "  -> darwin/arm64..."
	@GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/proxy

	@echo "  -> darwin/amd64..."
	@GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/proxy

	@echo "  -> linux/amd64..."
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/proxy

	@echo "  -> linux/arm64..."
	@GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/proxy

	@echo "  -> windows/amd64..."
	@GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/proxy

	@echo "All binaries built in $(BIN_DIR)/:"
	@ls -la $(BIN_DIR)/

install:
	@./scripts/install.sh

# gen/ is excluded from every gofmt target here. Its formatting comes from the
# protobuf generator, so reformatting it would be undone by the next
# scripts/generate-proto.sh — and it carries known drift today for that reason.
GOFMT_PATHS=$(shell find . -name '*.go' -not -path './gen/*' -not -path './.claude/*' -not -path './bin/*')

fmt:
	@gofmt -l -w $(GOFMT_PATHS)
	@echo "gofmt applied."

fmt-check:
	@drift="$$(gofmt -l $(GOFMT_PATHS))"; \
	if [ -n "$$drift" ]; then \
		echo "Not gofmt-clean:"; echo "$$drift"; \
		echo "Run: make fmt"; \
		exit 1; \
	fi; \
	echo "gofmt clean."

install-hooks:
	@git config core.hooksPath scripts/git-hooks
	@chmod +x scripts/git-hooks/*
	@echo "core.hooksPath -> scripts/git-hooks"
	@echo "Installed: $$(ls scripts/git-hooks)"
	@echo "Bypass once with: git commit --no-verify   (or SKIP_GOFMT_HOOK=1)"
