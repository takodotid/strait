SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
IMAGE ?= ghcr.io/takodotid/strait
TAG ?= dev

CONTROLLER_GEN := go tool controller-gen
# Sets the version and the commit that every binary reports.
VERSION_LDFLAGS = -X github.com/takodotid/strait/internal/version.Version=$(VERSION) -X github.com/takodotid/strait/internal/version.Commit=$(COMMIT)

.PHONY: all
all: generate build test

.PHONY: generate
generate: ## Generate deepcopy code and CRDs from api/, with v1alpha1 served as a deprecated copy of v1.
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd
	go run ./hack/crdversions config/crd
	rm -rf deploy/helm/strait/crds && mkdir -p deploy/helm/strait/crds && cp config/crd/*.yaml deploy/helm/strait/crds/
	cp deploy/envoy-bootstrap.yaml deploy/helm/strait/files/envoy-bootstrap.yaml

.PHONY: verify
verify: generate ## Fail if generated files are out of date.
	gofmt -l . | (! grep .)
	go vet ./...
	go vet -tags e2e ./test/e2e/...
	go vet -tags envoy ./internal/xds/
	git diff --exit-code -- api config deploy/helm/strait/crds deploy/helm/strait/files

.PHONY: build
build: ## Build every binary into bin/.
	CGO_ENABLED=0 go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o bin/ ./cmd/...

.PHONY: docs
docs: ## Write the man pages into bin/man and the skill document into bin/SKILL.md.
	go run ./hack/gendocs -skill bin/SKILL.md bin/man/man1

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
DIST := dist/strait_$(VERSION)_$(GOOS)_$(GOARCH)
EXE := $(if $(filter windows,$(GOOS)),.exe)
ARCHIVE := $(DIST)$(if $(filter windows,$(GOOS)),.zip,.tar.gz)
# The time of every entry of the CLI archive, and the date of its man pages: the commit's time, read once, so that two
# builds of one commit give the same archive (RFC0006 requirement 17). Without git, the current time.
ifndef SOURCE_DATE_EPOCH
SOURCE_DATE_EPOCH := $(shell git log -1 --format=%ct 2>/dev/null || date +%s)
endif
# Runs a tool of the build on the host, whatever GOOS and GOARCH the archive is for.
HOSTGO := GOOS=$(shell go env GOHOSTOS) GOARCH=$(shell go env GOHOSTARCH) go

.PHONY: dist
dist: ## Build the CLI release archive: strait, kubectl-strait, man pages, skill document. A .zip for windows, a .tar.gz otherwise. The same bytes for one commit, Go version and SOURCE_DATE_EPOCH.
	rm -rf $(DIST) $(DIST).zip $(DIST).tar.gz && mkdir -p $(DIST)/man/man1
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "-s -w $(VERSION_LDFLAGS)" -o $(DIST)/strait$(EXE) ./cmd/strait
	cp $(DIST)/strait$(EXE) $(DIST)/kubectl-strait$(EXE)
	SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) $(HOSTGO) run ./hack/gendocs -skill $(DIST)/SKILL.md $(DIST)/man/man1
	cp LICENSE README.md $(DIST)/
	SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) $(HOSTGO) run ./hack/archive $(ARCHIVE) $(DIST)

# The digest of the Strait image that make chart writes into the chart, sha256:... Empty: the chart names the image by
# its tag only.
IMAGE_DIGEST ?=

.PHONY: chart
chart: ## Package the chart into dist/ at VERSION, naming the Strait image by IMAGE_DIGEST when set.
	hack/release/chart.sh $(VERSION) $(IMAGE_DIGEST)

.PHONY: lint
lint: ## Verify the module's downloads, scan for known vulnerabilities, run staticcheck and helm lint, check action pins.
	go mod verify
	go tool -modfile=hack/tools/go.mod govulncheck ./...
	go tool -modfile=hack/tools/go.mod staticcheck ./...
	go tool -modfile=hack/tools/go.mod staticcheck -tags e2e ./test/e2e/...
	go tool -modfile=hack/tools/go.mod staticcheck -tags envoy ./internal/xds/
	helm lint --strict deploy/helm/strait --set site.name=a
	hack/release/pinned.sh

# The Kubernetes version of envtest's kube-apiserver and etcd, which run the CRDs' rules in make test.
ENVTEST_K8S_VERSION ?= 1.37.0

.PHONY: test
test: ## Run unit tests. Downloads envtest's API server into bin/envtest on first use.
	assets=$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest -p path); \
	KUBEBUILDER_ASSETS=$$assets go test ./...

.PHONY: test-envoy
test-envoy: ## Run the xDS configuration in a real Envoy, the chart's image, in docker.
	go test -tags envoy -count=1 ./internal/xds/

.PHONY: images
images: ## Build the container image.
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(TAG) .

.PHONY: e2e
e2e: images ## Run the end-to-end tests on kind (test/e2e/README.md). Creates and deletes strait-e2e-* clusters.
	STRAIT_IMAGE=$(IMAGE):$(TAG) go test -tags e2e -count=1 -timeout 60m -v ./test/e2e/...

.PHONY: bench
bench: ## Run the gateway benchmark on kind. Creates and deletes strait-bench-* clusters.
	bench/run.sh

.PHONY: help
help:
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-10s %s\n", $$1, $$2}'
