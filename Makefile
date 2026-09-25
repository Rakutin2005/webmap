BINARY  := webmap
PKG     := ./cmd/webmap
GO      ?= go
GOFLAGS ?=

.PHONY: all build test vet fmt fmtcheck lint check clean install run

all: build

## build: compile the webmap binary into the repository root
build:
	$(GO) build $(GOFLAGS) -o $(BINARY) $(PKG)
	@echo "Build complete: ./$(BINARY)"

## test: run the full test suite
test:
	$(GO) test $(GOFLAGS) ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## fmt: format all sources
fmt:
	$(GO) fmt ./...

## fmtcheck: fail if any source is not gofmt-clean
fmtcheck:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

## lint: formatting and vet checks
lint: fmtcheck vet

## check: formatting, vet and tests
check: lint test

## install: install the binary into GOBIN
install:
	$(GO) install $(GOFLAGS) $(PKG)

## run: build and run against a target, e.g. make run URL=https://example.com
run: build
	./$(BINARY) -url "$(URL)"

## clean: remove build artifacts
clean:
	rm -f $(BINARY)
	$(GO) clean
