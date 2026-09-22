MAKEFLAGS += --warn-undefined-variables

.ONESHELL:
.SHELLFLAGS := -o errtrace -o pipefail -o noclobber -o errexit -o nounset -c
SHELL := bash

# pinned; `go run` fetches the exact version on demand
GO_GOLANGCI_LINT_VERSION ?= 2.13.2
GO_GOLANGCI_LINT_TIMEOUT ?= 10m
PRE_COMMIT_VERSION ?= 4.6

GO_GOLANGCI_LINT_CMD ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GO_GOLANGCI_LINT_VERSION)
PRE_COMMIT ?= uvx pre-commit@$(PRE_COMMIT_VERSION)
SHELLCHECK_EXCLUDE := -path ./.git -o -path ./artifacts
GO_MOD_ID := github.com/grinderz/repo-tools
GO_LINE_LENGTH ?= 120

# test
GO_GOTESTSUM_VERSION ?= 1.13.0
GO_GOTESTSUM_ARGS ?= --format testname
GO_GOTESTSUM_CMD ?= go run gotest.tools/gotestsum@v$(GO_GOTESTSUM_VERSION) $(GO_GOTESTSUM_ARGS) --
GO_TEST_CMD ?= $(GO_GOTESTSUM_CMD)
GO_TEST_COVERAGE_EXCLUDE ?= (_mock\.go|\/testmocks\/|\.pb\.go)
GO_TEST_COVERAGE_THRESHOLD ?= 0
GO_TEST_REPEAT_COUNT ?= 2
# on: a random order per pass, the seed is printed on failure and can be
# passed back here to reproduce it
GO_TEST_SHUFFLE ?= on
# build tags for the tests, space or comma separated. A repeated -tags flag
# replaces the earlier one, so all tags are joined into a single flag here and
# test.coverage adds its own tag to the list instead of passing a second flag
GO_TEST_TAGS ?=
go-empty :=
go-space := $(go-empty) $(go-empty)
go-comma := ,
GO_TEST_TAGS_FLAG = $(if $(strip $(GO_TEST_TAGS)),-tags=$(subst $(go-space),$(go-comma),$(strip $(subst $(go-comma),$(go-space),$(GO_TEST_TAGS)))))

# the golang image test.docker runs in; the tests need nothing but go and git
GO_DOCKER_GO_VERSION ?= 1.26
GO_DOCKER_DEBIAN_VERSION ?= trixie
GO_DOCKER_IMAGE ?= golang:$(GO_DOCKER_GO_VERSION)-$(GO_DOCKER_DEBIAN_VERSION)
# runs before make inside the container
GO_DOCKER_SETUP_CMD ?= :
# named volumes: without them every run downloads the modules and rebuilds all
GO_DOCKER_MOD_CACHE_VOLUME ?= repo-tools-go-mod-cache
GO_DOCKER_BUILD_CACHE_VOLUME ?= repo-tools-go-build-cache

ARTIFACTS_DIR ?= artifacts
BIN ?= $(ARTIFACTS_DIR)/rt

ARGS ?=


.DEFAULT_GOAL := help


##@ General


.PHONY: help
help: ## Display this help screen
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9\-\\.%]+:.*?##/ { printf "  \033[36m%-29s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)


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


.PHONY: go.format
go.format: ## Format the source code
	go run github.com/segmentio/golines@latest --max-len=$(GO_LINE_LENGTH) --no-reformat-tags --ignore-generated --write-output .
	go run mvdan.cc/gofumpt@latest -l -w -modpath . .
	go run golang.org/x/tools/cmd/goimports@latest -l -w .
	go run github.com/daixiang0/gci@latest write --skip-generated -s standard -s default -s prefix\($(GO_MOD_ID)\) .

.PHONY: deps.update.all.patch
deps.update.all.patch: ## Update all deps to the latest patch
	GOPROXY=direct go get -u=patch ./...
	go mod tidy

.PHONY: deps.update.all.latest
deps.update.all.latest: ## Update all deps to the latest version
	GOPROXY=direct go get -u ./...
	go mod tidy


##@ Lint


.PHONY: lint
lint: lint.vet lint.golangci lint.shellcheck lint.pre-commit ## Run all linters

.PHONY: lint.vet
lint.vet: ## Run go vet
	go vet ./...

.PHONY: lint.golangci
lint.golangci: ## Run golangci-lint (ARGS=... for extra flags)
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

.PHONY: lint.fix
lint.fix: ## Run golangci-lint with --fix
	$(GO_GOLANGCI_LINT_CMD) run --timeout=$(GO_GOLANGCI_LINT_TIMEOUT) --fix --show-stats $(ARGS)

.PHONY: lint.shellcheck
lint.shellcheck: ## Run shellcheck on the repo's shell scripts
	find . -type d \( $(SHELLCHECK_EXCLUDE) \) -prune -o -type f -name '*.sh' \
		-exec shellcheck --format=gcc -s bash {} +

.PHONY: lint.pre-commit
lint.pre-commit: ## Run the pre-commit hooks on every file
	$(PRE_COMMIT) run --all-files


##@ Test


.PHONY: test
test: ## Run tests
	$(GO_TEST_CMD) -v -race $(GO_TEST_TAGS_FLAG) $(ARGS) $(GO_MOD_ID)/...

# runs every test N times in one process and in a shuffled order: state that
# leaks between runs only shows up on the second pass, and order dependence
# only in a different order, both stay invisible to a plain make test
.PHONY: test.repeat
test.repeat: override ARGS := -count=$(GO_TEST_REPEAT_COUNT) -shuffle=$(GO_TEST_SHUFFLE) $(ARGS)
test.repeat: test ## Run tests GO_TEST_REPEAT_COUNT times in one process, shuffled

# override: a command line GO_TEST_TAGS= or ARGS= extends these instead of
# replacing them, otherwise the coverage tag or the coverprofile flag is lost
.PHONY: test.coverage
test.coverage: override GO_TEST_TAGS := coverage $(GO_TEST_TAGS)
test.coverage: override ARGS := -coverpkg=$(GO_MOD_ID)/... -covermode=atomic -coverprofile=$(ARTIFACTS_DIR)/coverage.out.tmp $(ARGS)
test.coverage: $(ARTIFACTS_DIR) test ## Run tests with coverage report, fails under GO_TEST_COVERAGE_THRESHOLD
	grep -vE "$(GO_TEST_COVERAGE_EXCLUDE)" $(ARTIFACTS_DIR)/coverage.out.tmp >| $(ARTIFACTS_DIR)/coverage.out
	go tool cover -html=$(ARTIFACTS_DIR)/coverage.out -o $(ARTIFACTS_DIR)/coverage.html
	go tool cover -func=$(ARTIFACTS_DIR)/coverage.out
	./scripts/check-coverage.sh $(ARTIFACTS_DIR)/coverage.out $(GO_TEST_COVERAGE_THRESHOLD)

# $(1): make target to run inside the container. ARGS and GO_TEST_TAGS go in
# as env vars, not command line overrides: an override would replace the
# target-specific values of test.coverage.
# Signals: sh defers a trap while a foreground command runs, so make runs in
# the background and wait picks the ctrl-c or job cancel up at once, the trap
# then forwards it to every process of the container. --init keeps a real
# pid 1 in front of sh, which reaps the orphans that leaves behind.
define go-docker-run
	docker run -t --rm --init \
		-e GOPROXY -e GONOSUMDB -e GOPRIVATE \
		-v "$$(pwd)":/app \
		-v $(GO_DOCKER_MOD_CACHE_VOLUME):/go/pkg/mod \
		-v $(GO_DOCKER_BUILD_CACHE_VOLUME):/root/.cache/go-build \
		-w /app \
		$(GO_DOCKER_IMAGE) \
		sh -c '$(GO_DOCKER_SETUP_CMD) || exit; \
			trap "chown -R $(shell id -u):$(shell id -g) $(ARTIFACTS_DIR) 2>/dev/null || :" EXIT; \
			trap "kill -s INT -- -1" INT; trap "kill -s TERM -- -1" TERM; \
			ARGS="$(ARGS)" GO_TEST_TAGS="$(GO_TEST_TAGS)" make $(1) & wait $$!; st=$$?; wait; exit $$st'
endef

.PHONY: test.docker
test.docker: ## Run tests in the golang image, ARGS= passes go test args
	$(call go-docker-run,test)

test.docker.%: ## Run make test.$* in the golang image, e.g. test.docker.coverage
	$(call go-docker-run,test.$*)
