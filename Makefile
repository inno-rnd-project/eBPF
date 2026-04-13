ARCH := $(shell uname -m)
BPFTOOL := $(shell command -v bpftool 2>/dev/null)
IMAGE ?= netobs-agent:0.1.0
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

.PHONY: deps generate build run clean tree image-build image-push deploy-dev deploy-prod

deps:
	go mod tidy

generate:
	@if [ -z "$(BPFTOOL)" ]; then echo "bpftool not found"; exit 1; fi
	$(BPFTOOL) btf dump file /sys/kernel/btf/vmlinux format c > ./bpf/vmlinux.h
	cd internal/ebpf && GOPACKAGE=ebpfx go run github.com/cilium/ebpf/cmd/bpf2go@v0.17.1 \
		-go-package ebpfx \
		-cc clang \
		-cflags "$(BPF_CFLAGS)" \
		NetObs ../../bpf/netlat.bpf.c -- -I../../bpf

build: generate
	go build -o ./bin/netobs-agent ./cmd/netobs-agent

run:
	sudo ./bin/netobs-agent -listen :9810 -print-events=true

image-build:
	docker build -t $(IMAGE) .

image-push:
	docker push $(IMAGE)

deploy-dev:
	kubectl apply -k deploy/overlays/dev

deploy-prod:
	kubectl apply -k deploy/overlays/prod

clean:
	rm -f ./bin/netobs-agent
	rm -f ./internal/ebpf/netobs_bpfel.go ./internal/ebpf/netobs_bpfeb.go
	rm -f ./internal/ebpf/netobs_bpfel.o  ./internal/ebpf/netobs_bpfeb.o

tree:
	find . -maxdepth 4 -type f | sort
