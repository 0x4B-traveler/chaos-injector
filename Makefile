# chaos-injector 开发命令（Python + Go 双实现）
# 用法：make py-lint py-test go-fmt go-vet go-test go-build

PY   ?= python
RUFF ?= ruff
GO   ?= go

.PHONY: py-lint py-test py-install go-fmt go-vet go-test go-build test lint build all

## ---- Python ----

py-install:
	cd python && $(PY) -m pip install -e ".[dev]"

py-lint:
	cd python && $(RUFF) check src tests

py-test:
	cd python && $(PY) -m pytest -q

## ---- Go ----

go-fmt:
	cd go && $(GO) fmt ./...

go-vet:
	cd go && $(GO) vet ./...

go-test:
	cd go && $(GO) test ./... -count=1

go-build:
	cd go && $(GO) build -o chaos-injector ./cmd/chaos-injector

## ---- 组合目标 ----

test: py-test go-test
lint: py-lint go-vet
build: go-build
all: lint test build
