# Copyright (c) 2026-Present Diagrid Inc.
# SPDX-License-Identifier: BUSL-1.1

# go-ai — common tasks
#
# Each module is self-contained (own go.mod/go.sum + replace directives), so we
# build them standalone. GOWORK=off keeps an unrelated parent go.work (common in
# a GOPATH checkout) from pulling them into a workspace.
export GOWORK := off

.PHONY: help tidy build test vet fmt langchaingo eino adapters

help:
	@echo "targets:"
	@echo "  make tidy         - resolve dependencies across all modules (needs network)"
	@echo "  make build        - build the core module (engine + Dapr backend)"
	@echo "  make test         - run unit tests (engine + registry; no sidecar needed)"
	@echo "  make langchaingo  - run the LangChainGo example on Catalyst"
	@echo "  make eino         - run the Eino example on Catalyst"
	@echo "  make adapters     - tidy + build the adapter modules"

tidy:
	go mod tidy
	cd adapters/langchaingo && go mod tidy
	cd adapters/eino && go mod tidy
	cd examples/langchaingo && go mod tidy
	cd examples/eino && go mod tidy
	cd examples/mcp && go mod tidy

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# Examples run on Catalyst. Set DAPR_GRPC_ENDPOINT + DAPR_API_TOKEN (or use
# `diagrid dev run`) and GOAI_REGISTRY_STORE first.
langchaingo:
	cd examples/langchaingo && go run .

eino:
	cd examples/eino && go run .

adapters:
	cd adapters/langchaingo && go mod tidy && go build ./...
	cd adapters/eino && go mod tidy && go build ./...
