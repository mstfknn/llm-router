APP_NAME := llm-proxy
BUILD_DIR := build
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: all build clean test vet fmt run release

all: test build

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME) .

run: build
	$(BUILD_DIR)/$(APP_NAME)

test:
	go test -v -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf $(BUILD_DIR)

release: clean test
	@mkdir -p $(BUILD_DIR)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		output=$(BUILD_DIR)/$(APP_NAME)-$${os}-$${arch}; \
		if [ "$${os}" = "windows" ]; then output=$${output}.exe; fi; \
		echo "Building $${os}/$${arch}..."; \
		GOOS=$${os} GOARCH=$${arch} CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $${output} .; \
	done
	@echo "Release binaries in $(BUILD_DIR)/"
	@ls -lh $(BUILD_DIR)/
