VERSION := $(shell cat VERSION)
ARCH := $(shell uname -m)
BPFTOOL := $(shell command -v bpftool 2>/dev/null)
IMAGE ?= netobs-agent:$(VERSION)
REGISTRY_IMAGE ?= ghcr.io/inno-rnd-project/netobs-agent:$(VERSION)
KUSTOMIZE ?= kubectl kustomize

ifeq ($(ARCH),x86_64)
TARGET_ARCH := x86
else ifeq ($(ARCH),aarch64)
TARGET_ARCH := arm64
else ifeq ($(ARCH),arm64)
TARGET_ARCH := arm64
else
TARGET_ARCH := $(ARCH)
endif

BPF_CFLAGS := -O2 -g -D__TARGET_ARCH_$(TARGET_ARCH)

.PHONY: deps generate build run clean tree image-build image-push \
	render-dev render-prod deploy-dev deploy-prod delete-dev delete-prod bump

deps:
	go mod tidy

generate:
	@if [ -z "$(BPFTOOL)" ]; then echo "bpftool not found"; exit 1; fi
	$(BPFTOOL) btf dump file /sys/kernel/btf/vmlinux format c > ./bpf/vmlinux.h && \
	cd internal/ebpf && GOPACKAGE=ebpfx go run github.com/cilium/ebpf/cmd/bpf2go@v0.17.1 \
	-go-package ebpfx \
	-cc clang \
	-cflags "$(BPF_CFLAGS)" \
	NetObs ../../bpf/netlat.bpf.c -- -I../../bpf

build: generate
	go fmt ./...
	go build -o ./bin/netobs-agent ./cmd/netobs-agent

run:
	sudo ./bin/netobs-agent -listen :9810 -print-events=true

image-build:
	docker build -t $(IMAGE) .

image-push:
	docker tag $(IMAGE) $(REGISTRY_IMAGE)
	docker push $(REGISTRY_IMAGE)

render-dev:
	$(KUSTOMIZE) deploy/overlays/dev

render-prod:
	$(KUSTOMIZE) deploy/overlays/prod

deploy-dev:
	kubectl apply -k deploy/overlays/dev

deploy-prod:
	kubectl apply -k deploy/overlays/prod

delete-dev:
	kubectl delete -k deploy/overlays/dev

delete-prod:
	kubectl delete -k deploy/overlays/prod

bump:
	@CUR=$$(cat VERSION); \
	MAJOR=$$(echo $$CUR | cut -d. -f1); \
	MINOR=$$(echo $$CUR | cut -d. -f2); \
	PATCH=$$(echo $$CUR | cut -d. -f3); \
	PATCH=$$((PATCH + 1)); \
	if [ "$$PATCH" -ge 10 ]; then PATCH=0; MINOR=$$((MINOR + 1)); fi; \
	if [ "$$MINOR" -ge 10 ]; then MINOR=0; MAJOR=$$((MAJOR + 1)); fi; \
	NEW="$$MAJOR.$$MINOR.$$PATCH"; \
	echo "$$NEW" > VERSION; \
	sed -i 's/newTag: ".*"/newTag: "'$$NEW'"/' deploy/overlays/dev/kustomization.yaml; \
	sed -i 's/newTag: ".*"/newTag: "'$$NEW'"/' deploy/overlays/prod/kustomization.yaml; \
	echo "bumped $$CUR -> $$NEW"

clean:
	rm -f ./bin/netobs-agent
	rm -f ./internal/ebpf/netobs_bpfel.go ./internal/ebpf/netobs_bpfeb.go
	rm -f ./internal/ebpf/netobs_bpfel.o  ./internal/ebpf/netobs_bpfeb.o

tree:
	find . -maxdepth 4 -type f | sort
