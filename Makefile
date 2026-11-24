GO ?= go
GO_FILES := $(shell find . -type f -name '*.go' -not -path './vendor/*' -not -path './.git/*')
BIN := $(CURDIR)/bin
GOLANGCI_LINT_VERSION := v2.15.0
NILAWAY_VERSION := v0.0.0-20261124161746-91ad9bf5d8e6
export GOBIN := $(BIN)

.PHONY: check fmt-check lint nilaway test test-race vet build dependencies

check: fmt-check dependencies lint nilaway test test-race vet build

fmt-check:
	@test -z "$$(gofmt -l $(GO_FILES))" || { \
		echo "gofmt required:" >&2; \
		gofmt -l $(GO_FILES) >&2; \
		exit 1; \
	}

lint: $(BIN)/golangci-lint
	$(BIN)/golangci-lint run

$(BIN)/golangci-lint: go.mod .golangci.yml Makefile | $(BIN)
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

nilaway: $(BIN)/nilaway
	$(BIN)/nilaway -exclude-test-files ./...

$(BIN)/nilaway: Makefile | $(BIN)
	$(GO) install go.uber.org/nilaway/cmd/nilaway@$(NILAWAY_VERSION)

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

dependencies:
	$(GO) mod verify
	$(GO) list -m -mod=readonly all >/dev/null

$(BIN):
	mkdir -p $@
