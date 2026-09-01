IMAGE ?= ghcr.io/tinkerbell-community/tinkerbell-bmc-discovery-controller
TAG ?= latest

.PHONY: build
build:
	CGO_ENABLED=0 go build -o bin/manager ./cmd

.PHONY: test
test:
	CGO_ENABLED=1 go test -race -coverprofile=cover.out ./...

# Field-ownership tests against a real API server (envtest). Managed-fields
# semantics are asserted here, not with the fake client — see
# docs/discovery-field-ownership.md.
ENVTEST_K8S_VERSION ?= 1.37.0
.PHONY: test-envtest
test-envtest:
	KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.24 use $(ENVTEST_K8S_VERSION) -p path)" \
		CGO_ENABLED=1 go test -race -tags envtest -run 'TestEnvtest' ./internal/sync/...

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt-check
fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

.PHONY: lint
lint:
	golangci-lint run

# Build binaries and container images locally via goreleaser without
# publishing or signing anything.
.PHONY: snapshot
snapshot:
	goreleaser release --clean --snapshot --skip=sign,publish

.PHONY: helm-lint
helm-lint:
	helm lint helm/tinkerbell-bmc-discovery-controller
