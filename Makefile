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
	$(CONTROLLER_GEN) rbac:roleName=strait-controller paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
	rm -rf deploy/helm/strait/crds && mkdir -p deploy/helm/strait/crds && cp config/crd/*.yaml deploy/helm/strait/crds/

.PHONY: verify
verify: generate ## Fail if generated files are out of date.
	gofmt -l . | (! grep .)
	go vet ./...
	git diff --exit-code -- api config deploy/helm/strait/crds

.PHONY: build
build: ## Build every binary into bin/.
	CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/takodotid/strait/internal/version.Version=$(VERSION)" -o bin/ ./cmd/...

.PHONY: docs
docs: ## Write the man pages into bin/man and the skill document into bin/SKILL.md.
	go run ./hack/gendocs -man bin/man/man1 -skill bin/SKILL.md

.PHONY: test
test: ## Run unit tests.
	go test ./...

.PHONY: images
images: ## Build the container image.
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .

.PHONY: e2e
e2e: images ## Run the end-to-end tests on kind. Creates and deletes strait-e2e-* clusters.
	STRAIT_IMAGE=$(IMAGE):$(TAG) go test -tags e2e -count=1 -timeout 90m -v ./test/e2e/...

.PHONY: help
help:
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-10s %s\n", $$1, $$2}'
