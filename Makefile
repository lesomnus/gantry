# gantry — common developer tasks. See docs/e2e-testing.md for the E2E tiers.

GO ?= go
COMPOSE ?= docker compose -f deploy/compose/e2e.compose.yaml
REG ?= registry:2

.PHONY: build test vet fmt \
	e2e e2e-daemon e2e-registries e2e-blackbox e2e-image e2e-up e2e-down e2e-seed e2e-infra

## build: host build of the binary
build:
	CGO_ENABLED=0 $(GO) build ./...

## test: unit tests + the hermetic L1 E2E suite (no infra)
test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal tools

## e2e: L1 hermetic E2E only (fast, no daemon)
e2e:
	$(GO) test -race ./internal/e2e/...

## e2e-daemon: L2 real-daemon E2E (needs a reachable docker daemon)
e2e-daemon:
	$(GO) test -tags e2e -race -run TestL2 ./internal/e2e/...

## e2e-registries: L2 against a chosen registry image, e.g. make e2e-registries REG=registry:3
e2e-registries:
	GANTRY_E2E_REGISTRY=$(REG) $(GO) test -tags e2e -race -run TestL2 ./internal/e2e/...

## e2e-blackbox: L3 black-box binary (builds & runs the shipped binary; set
## GANTRY_E2E_BIN=<path> to reuse a prebuilt binary instead of recompiling)
e2e-blackbox:
	$(GO) test -tags e2e -run TestL3BlackBox ./internal/e2e/...

## e2e-image: L3 image tier — drives the actual built container image
## (set GANTRY_E2E_IMAGE=<ref>, e.g. one loaded from `docker buildx bake app`)
e2e-image:
	$(GO) test -tags e2e -run TestL3Image ./internal/e2e/...

## e2e-up: bring up the persistent multi-registry env (remote/cache/zot/proxy)
e2e-up:
	$(COMPOSE) up -d

## e2e-down: tear down the persistent env
e2e-down:
	$(COMPOSE) down -v

## e2e-seed: push a synthetic (optionally signed) image into the env's remote
e2e-seed:
	$(GO) run ./tools/e2e-seed --to 127.0.0.1:5001 --repo lib/app --tag 1 --insecure --sign --ca-out /tmp/gantry-e2e-ca.crt

## e2e-infra: provision the self-hosted infra tier and run the L3-infra tests
e2e-infra:
	ansible-playbook -i ansible/inventory.ini ansible/site.yml
	GANTRY_E2E_CONFIG=ansible/.out/gantry-e2e.json $(GO) test -tags e2e_infra -run TestL3Infra ./internal/e2e/...
