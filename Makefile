IMAGE ?= ghcr.io/tinkerbell-community/tinkerbell-bmc-discovery-controller
TAG ?= latest

.PHONY: build
build:
	CGO_ENABLED=0 go build -o bin/manager ./cmd

.PHONY: test
test:
	go test -race -coverprofile=cover.out ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt-check
fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

.PHONY: docker-build
docker-build:
	docker build -t $(IMAGE):$(TAG) .

.PHONY: helm-lint
helm-lint:
	helm lint helm/tinkerbell-bmc-discovery-controller
