BINARY ?= oxide-github-actions-runner-scaleset
GOLANGCI_LINT := go tool -modfile=tools/go.mod golangci-lint

.PHONY: check fmt fmt-check lint lint-check tidy tidy-check test build

check: fmt-check lint-check tidy-check
	+$(MAKE) test build

fmt:
	$(GOLANGCI_LINT) fmt

fmt-check:
	$(GOLANGCI_LINT) fmt --diff

lint:
	$(GOLANGCI_LINT) run --fix ./...

lint-check:
	$(GOLANGCI_LINT) run ./...

tidy:
	go mod tidy
	go -C tools mod tidy

tidy-check:
	go mod tidy -diff
	go -C tools mod tidy -diff

test:
	go test ./...

build:
	go build -trimpath -o $(BINARY) .
