# Incidentary Bridge — developer Makefile.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -ec -o pipefail

CHART_NAME      := incidentary-bridge
CHART_DIR       := charts/$(CHART_NAME)
IMAGE           ?= incidentary/bridge:dev
GO_PACKAGES     := ./...
BRIDGE_PACKAGE  := ./cmd/incidentaryprocessor

# Per the operator chart's convention, generate a tiny help table from
# `## descriptions` so `make` and `make help` stay readable as targets grow.

.PHONY: all
all: build

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\n"} \
	      /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
	      /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build & test

.PHONY: build
build: ## Build the standalone Bridge binary.
	go build -trimpath -o bin/incidentaryprocessor $(BRIDGE_PACKAGE)

.PHONY: test
test: ## Run unit tests.
	go test -race -count=1 $(GO_PACKAGES)

.PHONY: vet
vet: ## Run go vet.
	go vet $(GO_PACKAGES)

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run

.PHONY: integration
integration: ## Run the end-to-end synthetic-stream test.
	go test -count=1 ./test/integration/...

##@ Container

.PHONY: docker-build
docker-build: ## Build the Bridge container image (single-arch, local).
	docker build -t $(IMAGE) .

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the Bridge Helm chart.
	helm lint $(CHART_DIR) --set apiKey=test --set cluster.name=test

.PHONY: helm-template
helm-template: ## Render the Bridge Helm chart with the same values used in CI.
	helm template test $(CHART_DIR) --set apiKey=test --set cluster.name=test

##@ Housekeeping

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf _otelcol/ bin/
