SHELL := /usr/bin/env bash
IMAGE    ?= otel-slo-traffic-monitor
TAG      ?= dev
REGISTRY ?=
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
OVERLAY  ?= minikube
SLO_CONFIG := deploy/base/slo-engine/slo-config.yaml

.PHONY: help
help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n",$$1,$$2}'

## ---------- Go ----------
.PHONY: build test lint vuln
build: ## Build all binaries into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/version.Version=$(VERSION)" -o bin/ ./cmd/...

test: ## Unit tests with race detector and coverage
	OTEL_SDK_DISABLED=true go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

lint: ## gofmt + go vet
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)
	go vet ./...

vuln: ## govulncheck
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## ---------- Generated artifacts ----------
.PHONY: rules dashboard generate verify-generated
rules: ## Regenerate Prometheus SLO rules from the SLO config
	go run ./cmd/slo-engine rules -config $(SLO_CONFIG) > deploy/base/prometheus/slo-rules.yaml

dashboard: ## Regenerate the Grafana dashboard JSON
	python3 hack/gen-dashboard.py

generate: rules dashboard ## Regenerate everything

verify-generated: generate ## Fail if generated files are stale
	git diff --exit-code -- deploy/base/prometheus/slo-rules.yaml deploy/base/grafana/dashboards/

## ---------- Kubernetes ----------
.PHONY: manifests minikube-up image deploy wait e2e chaos heal port-forward undeploy minikube-down
manifests: ## Render + schema-validate every overlay (needs kustomize, kubeconform)
	@for o in minikube eks gke aks; do echo "== $$o"; kustomize build deploy/overlays/$$o | kubeconform -strict -summary -; done

minikube-up: ## Start a minikube cluster sized for the full stack
	minikube start --cpus=4 --memory=8192 --kubernetes-version=stable

image: ## Build with local Docker and load into minikube (no registry needed)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .
	minikube image load $(IMAGE):$(TAG)

image-push: ## Build and push a multi-arch image to $(REGISTRY)
	docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=$(VERSION) -t $(REGISTRY)/$(IMAGE):$(VERSION) --push .

deploy: ## kubectl apply the selected overlay (OVERLAY=minikube|eks|gke|aks)
	kubectl apply -k deploy/overlays/$(OVERLAY)

wait: ## Wait for every workload to become ready
	./scripts/wait-ready.sh

e2e: ## End-to-end test against the current kube context
	./scripts/e2e-minikube.sh

chaos: ## Inject 30% errors into payment and slow down catalog
	./scripts/chaos.sh inject

heal: ## Remove injected faults
	./scripts/chaos.sh heal

port-forward: ## Grafana :3000, Prometheus :9090, Jaeger :16686, SLO API :8081
	./scripts/port-forward.sh

undeploy: ## Delete the stack
	kubectl delete -k deploy/overlays/$(OVERLAY) --ignore-not-found

minikube-down: ## Delete the minikube cluster
	minikube delete

.PHONY: all
all: lint test build manifests ## CI-equivalent local checks
