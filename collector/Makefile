# Tags are vX.Y.Z; images and the chart use X.Y.Z.
VERSION   ?= $(or $(shell git describe --tags --always --dirty 2>/dev/null | sed "s/^v//"),dev)
IMAGE     ?= klique-idle-collector
PLATFORMS ?= linux/amd64,linux/arm64
KIND_NAME ?= klique-idle-dev
DIST      := dist
CHART     := ../charts/klique-idle

# NVIDIA's header marks some functions deprecated; we don't call them.
export CGO_CFLAGS := -Wno-deprecated-declarations

.PHONY: help test vet build run-fake hub image image-local dist bundle examples kind-demo kind-clean

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

test: ## unit tests with the race detector
	go test -race ./...

vet: ## go vet + gofmt check
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)

build: ## binary for this machine in bin/ (Linux binaries: make dist)
	go build -ldflags "-X main.version=$(VERSION)" -o bin/klique-idle-collector ./cmd/klique-idle-collector
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o bin/klique-idle-hub ./cmd/klique-idle-hub

run-fake: ## print reports from 8 simulated GPUs every 10 s (works on macOS)
	go run ./cmd/klique-idle-collector --gpu fake --pretty --health-addr off

hub: ## the cluster hub on :8080 (collectors post to http://localhost:8080/v1/reports; metrics on /metrics)
	go run ./cmd/klique-idle-hub --dump

image: ## multi-arch image; add PUSH=1 to push to $(IMAGE)
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) $(if $(PUSH),--push,) .

image-local: ## image for this machine's architecture, loaded into Docker as $(IMAGE):dev
	docker buildx build --build-arg VERSION=$(VERSION) -t $(IMAGE):dev --load .

dist: ## Linux binaries for amd64 and arm64 in dist/
	rm -rf $(DIST)
	docker buildx build --platform $(PLATFORMS) --target artifacts --build-arg VERSION=$(VERSION) \
		--output type=local,dest=$(DIST) .

bundle: dist ## per-arch tarballs with binary + install.sh + systemd unit, and SHA256SUMS
	@set -e; cd $(DIST); for d in linux_*; do \
		arch=$${d#linux_}; name=klique-idle-collector-$(VERSION)-linux-$$arch; \
		mkdir -p $$name; \
		cp $$d/klique-idle-collector $$name/klique-idle-collector_linux_$$arch; \
		cp $$d/klique-idle-hub $$name/klique-idle-hub_linux_$$arch; \
		cp ../../deploy/systemd/install.sh ../../deploy/systemd/klique-idle-collector.service $$name/; \
		COPYFILE_DISABLE=1 tar --no-xattrs -czf $$name.tar.gz $$name; rm -rf $$name; \
	done; sha256sum *.tar.gz > SHA256SUMS 2>/dev/null || shasum -a 256 *.tar.gz > SHA256SUMS
	@ls -l $(DIST)/*.tar.gz && cat $(DIST)/SHA256SUMS

examples: dist ## regenerate docs/examples/report-simulated-4gpu.json from the current code, then check all examples
	@# Runs on Linux (for real host stats). The node name seeds the simulator: gpu-node-03
	@# starts with an idle, a busy and two memory-held GPUs.
	docker run --rm --platform linux/amd64 -v $(CURDIR)/$(DIST)/linux_amd64:/opt/kic:ro ubuntu:24.04 \
		/opt/kic/klique-idle-collector --once --gpu fake --fake-gpus 4 --fake-gpu-model h100 \
		--scheduler kubernetes --cluster demo --node-name gpu-node-03 --labels site=lab \
		2>/dev/null > ../docs/examples/report-simulated-4gpu.json
	go test -count=1 -run 'Schema|Examples' ./internal/report/

kind-demo: image-local ## local 3-node kind cluster with simulated GPUs and the hub
	kind get clusters | grep -qx $(KIND_NAME) || \
		printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n- role: worker\n- role: worker\n' | \
		kind create cluster --name $(KIND_NAME) --config -
	kind load docker-image $(IMAGE):dev --name $(KIND_NAME)
	helm upgrade --install klique-idle $(CHART) --kube-context kind-$(KIND_NAME) \
		-n klique-idle --create-namespace \
		--set cluster=kind-demo --set image.repository=$(IMAGE) --set image.tag=dev --set image.pullPolicy=Never \
		--set collector.gpu=fake --set nodeSelector=null --set token=demo-token
	@echo "metrics: kubectl --context kind-$(KIND_NAME) -n klique-idle port-forward svc/klique-idle-hub 8080   # then: curl -s localhost:8080/metrics"

kind-clean: ## delete the kind cluster
	kind delete cluster --name $(KIND_NAME)
