BINARY    := sqlitem
BUILD_DIR := build
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOFLAGS   := -trimpath

# modernc.org/sqlite is pure Go, so every target is a static CGO-free build.
export CGO_ENABLED := 0

.PHONY: all build build-linux-arm64 build-linux-amd64 build-linux test vet clean

all: build

## build: build for the current OS/arch -> build/sqlitem
build:
	@mkdir -p $(BUILD_DIR)
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) .

## build-linux-arm64: -> build/sqlitem-linux-arm64
build-linux-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-linux-arm64 .

## build-linux-amd64: -> build/sqlitem-linux-amd64
build-linux-amd64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-linux-amd64 .

## build-linux: both linux targets
build-linux: build-linux-arm64 build-linux-amd64

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf $(BUILD_DIR)
