GO ?= go
DOCKER ?= docker
VERSION ?= dev
IMAGE ?= sentry-adapter:local
export IMAGE
SENTRY_PROXY_NETWORK ?=
export SENTRY_PROXY_NETWORK
SENTRY_ADAPTER_IPV4 ?=
export SENTRY_ADAPTER_IPV4

ifneq ($(strip $(SENTRY_ADAPTER_IPV4)),)
ifeq ($(strip $(SENTRY_PROXY_NETWORK)),)
$(error SENTRY_ADAPTER_IPV4 requires SENTRY_PROXY_NETWORK)
endif
endif

PROXY_COMPOSE_FILE := $(if $(strip $(SENTRY_PROXY_NETWORK)),-f compose.proxy.yaml,)
STATIC_IP_COMPOSE_FILE := $(if $(strip $(SENTRY_ADAPTER_IPV4)),-f compose.static-ip.yaml,)
BASE_COMPOSE_FILES := -f compose.yaml $(PROXY_COMPOSE_FILE) $(STATIC_IP_COMPOSE_FILE)
REAL_COMPOSE_FILES := -f compose.yaml -f compose.real.yaml $(PROXY_COMPOSE_FILE) $(STATIC_IP_COMPOSE_FILE)

ifeq ($(OS),Windows_NT)
BIN_EXT := .exe
else
BIN_EXT :=
endif

.PHONY: help fmt test vet check build probe-build release docker-build docker-push deploy deploy-image mock-deploy deploy-real deploy-mock status logs stop teambition-check teambition-check-docker swagger

help:
	@echo "make fmt           Format Go source files"
	@echo "make test          Run Go tests"
	@echo "make vet           Run Go static checks"
	@echo "make check         Run tests and static checks"
	@echo "make build         Build the local binary in bin/"
	@echo "make probe-build   Build the local Teambition connectivity probe"
	@echo "make release       Build Linux binaries and checksums in dist/VERSION/"
	@echo "make teambition-check  Probe private appToken once without showing credentials"
	@echo "make teambition-check-docker  Probe appToken from the adapter Compose network"
	@echo "make swagger       Start loopback Swagger UI at http://127.0.0.1:8790/swagger/"
	@echo "make docker-build  Build a container image (IMAGE=$(IMAGE))"
	@echo "make docker-push   Push IMAGE to its configured registry"
	@echo "make deploy-mock   Build and start the mock Compose stack"
	@echo "make deploy-real   Build and start the real Compose stack"
	@echo "make deploy-image  Start the real Compose stack from IMAGE without rebuilding"
	@echo "                   Set ADAPTER_BIND_IP in .env to the node IP when exposing port 8787 through a CLB"
	@echo "                   Set SENTRY_PROXY_NETWORK to join an existing Sentry/proxy network"
	@echo "                   Also set SENTRY_ADAPTER_IPV4 for a fixed IP on that network"
	@echo "make status        Show Compose service status"
	@echo "make logs          Follow adapter logs"
	@echo "make stop          Stop the Compose stack (preserve database volume)"

fmt:
	"$(GO)" fmt ./...

test:
	"$(GO)" test ./...

vet:
	"$(GO)" vet ./...

check: test vet

build:
	"$(GO)" build -trimpath -buildvcs=false -o "bin/sentry-adapter$(BIN_EXT)" ./cmd/adapter

probe-build:
	"$(GO)" build -trimpath -buildvcs=false -o "bin/teambition-probe$(BIN_EXT)" ./cmd/teambition-probe

release: check
	"$(GO)" run ./scripts/release -go "$(GO)" -version "$(VERSION)"

teambition-check:
	$(if $(wildcard .env),,$(error Missing .env; copy .env.example to .env and configure it))
	$(if $(wildcard config/credentials.env),,$(error Missing config/credentials.env; create it from the credential template))
	"$(GO)" run ./cmd/teambition-probe -env-file .env -credentials-file config/credentials.env -once

teambition-check-docker:
	$(if $(wildcard .env),,$(error Missing .env; copy .env.example to .env and configure it))
	$(if $(wildcard config/credentials.env),,$(error Missing config/credentials.env; create it from the credential template))
	"$(DOCKER)" compose -f compose.probe.yaml run --rm --no-deps --build -T teambition-probe

swagger:
	$(if $(wildcard .env),,$(error Missing .env; copy .env.example to .env and configure it))
	$(if $(wildcard config/credentials.env),,$(error Missing config/credentials.env; create it from the credential template))
	"$(GO)" run ./cmd/teambition-probe -env-file .env -credentials-file config/credentials.env -listen 127.0.0.1:8790

docker-build:
	"$(DOCKER)" build $(if $(strip $(GOPROXY)),--build-arg GOPROXY,) --tag "$(IMAGE)" .

docker-push:
	$(if $(filter sentry-adapter:local,$(IMAGE)),$(error Set IMAGE to a tagged registry image before pushing))
	"$(DOCKER)" push "$(IMAGE)"

mock-deploy:
	$(if $(wildcard .env),,$(error Missing .env; copy .env.example to .env and configure it))
	"$(DOCKER)" compose $(BASE_COMPOSE_FILES) config --quiet
	"$(DOCKER)" compose $(BASE_COMPOSE_FILES) up -d --build

deploy:
	$(if $(wildcard .env),,$(error Missing .env; copy .env.example to .env and configure it))
	$(if $(wildcard config/credentials.env),,$(error Missing config/credentials.env; create it from the credential template))
	"$(DOCKER)" compose $(REAL_COMPOSE_FILES) config --quiet
	"$(DOCKER)" compose $(REAL_COMPOSE_FILES) up -d --build

deploy-image:
	$(if $(wildcard .env),,$(error Missing .env; copy .env.example to .env and configure it))
	$(if $(wildcard config/credentials.env),,$(error Missing config/credentials.env; create it from the credential template))
	"$(DOCKER)" compose $(REAL_COMPOSE_FILES) config --quiet
	"$(DOCKER)" image inspect --format "{{.Id}}" "$(IMAGE)"
	"$(DOCKER)" compose $(REAL_COMPOSE_FILES) up -d --no-build --pull missing

deploy-mock: mock-deploy

deploy-real: deploy

status:
	"$(DOCKER)" compose $(BASE_COMPOSE_FILES) ps

logs:
	"$(DOCKER)" compose $(BASE_COMPOSE_FILES) logs --follow --tail=100 adapter

stop:
	"$(DOCKER)" compose $(BASE_COMPOSE_FILES) down
