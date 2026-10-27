SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= ghcr.io/takodotid/strait
TAG ?= dev

CONTROLLER_GEN := go tool controller-gen

.PHONY: all
all: generate build test

.PHONY: generate
generate: ## Generate deepcopy code and CRDs from api/.
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd
	rm -rf deploy/helm/strait/crds && mkdir -p deploy/helm/strait/crds && cp config/crd/*.yaml deploy/helm/strait/crds/
	cp deploy/envoy-bootstrap.yaml deploy/helm/strait/files/envoy-bootstrap.yaml

.PHONY: verify
verify: generate ## Fail if generated files are out of date.
	gofmt -l . | (! grep .)
	go vet ./...
	go vet -tags e2e ./test/e2e/...
	git diff --exit-code -- api config deploy/helm/strait/crds deploy/helm/strait/files

.PHONY: build
build: ## Build every binary into bin/.
	CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/takodotid/strait/internal/version.Version=$(VERSION)" -o bin/ ./cmd/...

.PHONY: docs
docs: ## Write the man pages into bin/man and the skill document into bin/SKILL.md.
	go run ./hack/gendocs -skill bin/SKILL.md bin/man/man1

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
DIST := dist/strait_$(VERSION)_$(GOOS)_$(GOARCH)

.PHONY: dist
dist: ## Build the CLI release archive: strait, kubectl-strait, man pages, skill document.
	rm -rf $(DIST) && mkdir -p $(DIST)/man/man1
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "-s -w -X github.com/takodotid/strait/internal/version.Version=$(VERSION)" -o $(DIST)/strait ./cmd/strait
	cp $(DIST)/strait $(DIST)/kubectl-strait
	GOOS=$(shell go env GOHOSTOS) GOARCH=$(shell go env GOHOSTARCH) go run ./hack/gendocs -skill $(DIST)/SKILL.md $(DIST)/man/man1
	cp LICENSE README.md $(DIST)/
	tar -C dist -czf $(DIST).tar.gz $(notdir $(DIST))

.PHONY: test
test: ## Run unit tests.
	go test ./...

.PHONY: images
images: ## Build the container image.
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .

.PHONY: e2e
e2e: images ## Run the end-to-end tests on kind (test/e2e/README.md). Creates and deletes strait-e2e-* clusters.
	STRAIT_IMAGE=$(IMAGE):$(TAG) go test -tags e2e -count=1 -timeout 60m -v ./test/e2e/...

.PHONY: bench
bench: ## Run the gateway benchmark on kind. Creates and deletes strait-bench-* clusters.
	bench/run.sh

.PHONY: help
help:
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-10s %s\n", $$1, $$2}'
