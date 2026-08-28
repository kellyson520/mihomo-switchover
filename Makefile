REPO_DIR := $(abspath .)
CONTAINER ?= mihomo-cliproxy
GO_IMAGE ?= golang:1.24-alpine

.PHONY: test vet build check

test:
		docker run --rm --network container:$(CONTAINER) \
		-e HTTPS_PROXY=http://127.0.0.1:7890 -e HTTP_PROXY=http://127.0.0.1:7890 \
		-v $(REPO_DIR):/src -w /src $(GO_IMAGE) sh -c 'go test -mod=vendor ./...'

vet:
	docker run --rm --network container:$(CONTAINER) \
		-e HTTPS_PROXY=http://127.0.0.1:7890 -e HTTP_PROXY=http://127.0.0.1:7890 \
		-v $(REPO_DIR):/src -w /src $(GO_IMAGE) sh -c 'go vet -mod=vendor ./...'

check: test vet

build:
	mkdir -p dist
	docker run --rm --network container:$(CONTAINER) \
		-e HTTPS_PROXY=http://127.0.0.1:7890 -e HTTP_PROXY=http://127.0.0.1:7890 \
		-v $(REPO_DIR):/src -w /src $(GO_IMAGE) \
		sh -c 'go test -mod=vendor ./... && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -trimpath -ldflags="-s -w" -o dist/guardian ./cmd/guardian'
