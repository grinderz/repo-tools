GO_GOLANGCI_LINT_VERSION ?= 2.12.2
GO_GOLANGCI_LINT_TIMEOUT ?= 10m
GO_GOLINES_VERSION ?= 0.13.0
GO_GOFUMPT_VERSION ?= 0.9.1
GO_GOIMPORTS_VERSION ?= 0.39.0
GO_GCI_VERSION ?= 0.13.7

GO_GOLANGCI_LINT_CMD ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GO_GOLANGCI_LINT_VERSION)
GO_MOD_ID := github.com/grinderz/repo-tools
GO_LINE_LENGTH ?= 120

ARTIFACTS_DIR ?= artifacts
BIN ?= $(ARTIFACTS_DIR)/rt

ARGS ?=


.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} \
		/^##@/ { printf "\n%s\n", substr($$0, 5) } \
		/^[a-zA-Z0-9_.%-]+:.*?##/ { printf "  %-28s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)


##@ Build


$(ARTIFACTS_DIR):
	mkdir -p $(ARTIFACTS_DIR)

.PHONY: build
build: $(ARTIFACTS_DIR) ## Build the rt binary into artifacts/
	go build -o $(BIN) ./cmd/rt

.PHONY: install
install: ## Install rt into GOBIN
	go install ./cmd/rt

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(ARTIFACTS_DIR)


##@ Development


.PHONY: format
format: ## Format the source code
	go run github.com/segmentio/golines@v$(GO_GOLINES_VERSION) --max-len=$(GO_LINE_LENGTH) --no-reformat-tags --ignore-generated --write-output .
	go run mvdan.cc/gofumpt@v$(GO_GOFUMPT_VERSION) -l -w -modpath . .
	go run golang.org/x/tools/cmd/goimports@v$(GO_GOIMPORTS_VERSION) -l -w .
	go run github.com/daixiang0/gci@v$(GO_GCI_VERSION) write --skip-generated -s standard -s default -s prefix\($(GO_MOD_ID)\) .

.PHONY: deps.update.all.patch
deps.update.all.patch: ## Update all deps to the latest patch
	GOPROXY=direct go get -u=patch ./...
	go mod tidy

.PHONY: deps.update.all.latest
deps.update.all.latest: ## Update all deps to the latest version
	GOPROXY=direct go get -u ./...
	go mod tidy


##@ Lint


.PHONY: lint.golangci
lint.golangci: ## Run golangci-lint
	$(GO_GOLANGCI_LINT_CMD) run --timeout=$(GO_GOLANGCI_LINT_TIMEOUT) --show-stats $(ARGS)

.PHONY: lint.golangci.bin
lint.golangci.bin: ## Run golangci-lint from PATH
	golangci-lint run --timeout=$(GO_GOLANGCI_LINT_TIMEOUT) --show-stats $(ARGS)

.PHONY: lint.docker.golangci
lint.docker.golangci: ## Run golangci-lint in docker
	docker run -t --rm \
		-v $$(pwd):/app \
		-v ~/.cache/golangci-lint/v$(GO_GOLANGCI_LINT_VERSION):/root/.cache \
		-w /app \
		golangci/golangci-lint:v$(GO_GOLANGCI_LINT_VERSION) \
		make lint.golangci.bin

.PHONY: lint.fmt
lint.fmt: ## Check formatting without writing
	$(GO_GOLANGCI_LINT_CMD) fmt --diff

.PHONY: lint.vet
lint.vet: ## Run go vet
	go vet ./...

.PHONY: lint.fix
lint.fix: ## Fix what the linters can fix
	$(GO_GOLANGCI_LINT_CMD) run --timeout=$(GO_GOLANGCI_LINT_TIMEOUT) --fix --show-stats $(ARGS)

.PHONY: lint
lint: lint.vet lint.golangci ## Run all linters


##@ Test


.PHONY: test
test: ## Run tests
	go test -race $(ARGS) ./...

.PHONY: test.coverage
test.coverage: $(ARTIFACTS_DIR) ## Run tests with a coverage report
	go test -race -covermode=atomic -coverprofile=$(ARTIFACTS_DIR)/coverage.out ./...
	go tool cover -html=$(ARTIFACTS_DIR)/coverage.out -o $(ARTIFACTS_DIR)/coverage.html
	go tool cover -func=$(ARTIFACTS_DIR)/coverage.out
