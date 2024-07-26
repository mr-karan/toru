BIN := ./bin/toru

LAST_COMMIT := $(shell git rev-parse --short HEAD)
LAST_COMMIT_DATE := $(shell git show -s --format=%ci ${LAST_COMMIT})
VERSION := $(shell git describe --tags)
BUILDSTR := ${VERSION} (Commit: ${LAST_COMMIT_DATE} (${LAST_COMMIT}), Build: $(shell date +"%Y-%m-%d% %H:%M:%S %z"))

.PHONY: build
build: ## Build the binary.
	CGO_ENABLED=0 go build -o ${BIN} -ldflags="-X 'main.buildString=${BUILDSTR}'" ./

.PHONY: run
run: build ## Build and run the binary.
	${BIN} --config config.toml

.PHONY: lint
lint: ## Run all the linters.
	golangci-lint run

.PHONY: dist
dist: build
	mkdir -p dist
	cp -R ${BIN} ./dist/
	cp -R ./config.toml ./dist/

.PHONY: clean
clean: ## Clean build artifacts.
	rm -rf ${BIN} dist

.PHONY: test
test: ## Run tests.
	go test ./...

.PHONY: help
help: ## Display this help screen.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'