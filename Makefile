SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

REGISTRY ?= harbor.szymonrichert.pl
IMAGE_NAME ?= containers/tatara-claude-code-wrapper
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CLAUDE_CODE_VERSION ?= latest
TATARA_CLI_VERSION ?= latest
IMAGE_REF := $(REGISTRY)/$(IMAGE_NAME):$(VERSION)
MODPATH := github.com/szymonrychu/tatara-claude-code-wrapper

.PHONY: all lint test build image push tidy fmt clean chart-test bump-claude

all: lint test build
tidy: ; go mod tidy
fmt:
	gofmt -s -w .
	goimports -w -local $(MODPATH) .
lint: ; golangci-lint run ./... || [ $$? -eq 5 ]
test: ; go test ./... -race -count=1
build:
	CGO_ENABLED=0 go build -trimpath \
	  -ldflags "-s -w -X $(MODPATH)/internal/version.Version=$(VERSION) -X $(MODPATH)/internal/version.Commit=$(COMMIT) -X $(MODPATH)/internal/version.Date=$(DATE)" \
	  -o bin/wrapper ./cmd/wrapper
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/cc-stop-hook ./cmd/cc-stop-hook
image:
	docker buildx build --platform=linux/amd64 \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
	  --build-arg CLAUDE_CODE_VERSION=$(CLAUDE_CODE_VERSION) --build-arg TATARA_CLI_VERSION=$(TATARA_CLI_VERSION) \
	  -t $(IMAGE_REF) --load .
push: image ; docker push $(IMAGE_REF)
chart-test: ; helm unittest charts/tatara-claude-code-wrapper
bump-claude:
	@test -n "$(VERSION_ARG)" || { echo "usage: make bump-claude VERSION_ARG=1.2.3"; exit 1; }
	sed -i '' 's/^ARG CLAUDE_CODE_VERSION=.*/ARG CLAUDE_CODE_VERSION=$(VERSION_ARG)/' Dockerfile
clean: ; rm -rf bin dist
