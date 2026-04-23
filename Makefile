VERSION := $(shell cat VERSION)
ARCH := $(shell uname -m)
BPFTOOL := $(shell command -v bpftool 2>/dev/null)
REGISTRY_BASE ?= ghcr.io/inno-rnd-project
KUSTOMIZE ?= kubectl kustomize

# ============================================================================
# Agent registry
# 새 에이전트 추가 시:
#   1) AGENTS에 이름 추가
#   2) PORT_<name>에 기본 포트 할당
#   3) 선행 태스크(BPF 재생성 등)가 필요하면 PREREQS_<name>에 타깃명 기입
#   4) CGO_<name>에 0(정적 바이너리) 또는 1(CGO 의존성 존재) 할당
# 이후 build-<name>, image-build-<name>, image-push-<name>이 자동으로 매치된다.
# ============================================================================
AGENTS := netobs-agent gpuobs-agent

PORT_netobs-agent := 9810
PORT_gpuobs-agent := 9820

PREREQS_netobs-agent := generate

# go-nvml v0.13.x는 NVML 호출을 CGO `import "C"`로 구현해 CGO 비활성 빌드에서 심볼이
# 해석되지 않는다. gpuobs-agent만 CGO=1로, netobs-agent는 기존 정적 바이너리 속성을 유지한다.
CGO_netobs-agent := 0
CGO_gpuobs-agent := 1

# ============================================================================
# Overlay registry — <agent-domain>-<rollout-stage> 형식
# 새 overlay 추가 시:
#   1) OVERLAYS에 이름 추가
#   2) OVERLAY_PATH_<name>에 kustomize 경로 지정
# 이후 render-<name>, deploy-<name>, delete-<name>이 자동으로 매치된다.
# ============================================================================
OVERLAYS := netobs-dev netobs-prod gpuobs-dev gpuobs-prod

OVERLAY_PATH_netobs-dev  := deploy/netobs/overlays/dev
OVERLAY_PATH_netobs-prod := deploy/netobs/overlays/prod
OVERLAY_PATH_gpuobs-dev  := deploy/gpuobs/overlays/dev
OVERLAY_PATH_gpuobs-prod := deploy/gpuobs/overlays/prod

# ============================================================================
# Architecture detection (netobs BPF 컴파일용)
# ============================================================================
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

# pattern rule(build-%-agent, image-*-%-agent, render/deploy/delete-%)로 매치되는 타깃은
# .PHONY에 넣지 않는다. GNU make는 .PHONY 타깃에 대해 implicit rule(pattern rule 포함)
# 탐색을 건너뛰므로 매치가 일어나지 않는다. 해당 타깃들은 동일 이름의 실제 파일이
# 없어 매 호출마다 recipe가 재실행되므로 phony와 동등 동작이다.
.PHONY: deps generate clean tree bump \
	build-all image-build-all image-push-all

# ============================================================================
# Core utilities
# ============================================================================
deps:
	go mod tidy

generate:
	@if [ -z "$(BPFTOOL)" ]; then echo "bpftool not found"; exit 1; fi
	$(BPFTOOL) btf dump file /sys/kernel/btf/vmlinux format c > ./bpf/vmlinux.h && \
	cd internal/netobs/ebpf && GOPACKAGE=ebpfx go run github.com/cilium/ebpf/cmd/bpf2go@v0.17.1 \
	-go-package ebpfx \
	-cc clang \
	-cflags "$(BPF_CFLAGS)" \
	NetObs ../../../bpf/netlat.bpf.c -- -I../../../bpf

# ============================================================================
# Agent build / image pipeline (pattern rule driven)
# .SECONDEXPANSION 덕분에 prerequisite에서 $$(PREREQS_$$*-agent)가 pattern 매치 이후에 평가되어
# 각 에이전트별 PREREQS_<name> 선언이 자동으로 선행 타깃으로 연결된다.
# netobs-agent처럼 BPF 재생성이 필요한 경우는 PREREQS_netobs-agent := generate 한 줄로 처리된다.
# ============================================================================
.SECONDEXPANSION:

build-%-agent: $$(PREREQS_$$*-agent)
	go fmt ./...
	CGO_ENABLED=$(CGO_$*-agent) go build -o ./bin/$*-agent ./cmd/$*-agent

image-build-%-agent:
	docker build \
		--build-arg TARGET_AGENT=$*-agent \
		--build-arg AGENT_PORT=$(PORT_$*-agent) \
		--build-arg CGO_ENABLED=$(CGO_$*-agent) \
		-t $*-agent:$(VERSION) .

image-push-%-agent: image-build-%-agent
	docker tag $*-agent:$(VERSION) $(REGISTRY_BASE)/$*-agent:$(VERSION)
	docker push $(REGISTRY_BASE)/$*-agent:$(VERSION)

# 우산 타깃. AGENTS 리스트를 순회해 모든 에이전트에 동일 작업을 일괄 수행한다.
build-all:       $(addprefix build-,$(AGENTS))
image-build-all: $(addprefix image-build-,$(AGENTS))
image-push-all:  $(addprefix image-push-,$(AGENTS))

# ============================================================================
# Overlay render / deploy / delete
# OVERLAY_PATH_<name> 변수를 lookup해 kustomize 경로를 주입한다.
# ============================================================================
render-%:
	$(KUSTOMIZE) $(OVERLAY_PATH_$*)

deploy-%:
	kubectl apply -k $(OVERLAY_PATH_$*)

delete-%:
	kubectl delete -k $(OVERLAY_PATH_$*)

# ============================================================================
# Version management
# deploy 하위 임의 경로의 overlay kustomization을 find로 자동 수집해 image tag를 갱신한다.
# 새 agent의 overlay가 추가돼도 bump 규칙 수정이 필요하지 않다.
# ============================================================================
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
	for f in $$(find deploy -type f -name kustomization.yaml -path '*/overlays/*' 2>/dev/null); do \
		sed -i 's/newTag: ".*"/newTag: "'$$NEW'"/' "$$f"; \
	done; \
	echo "bumped $$CUR -> $$NEW"

# ============================================================================
# Housekeeping
# ============================================================================
clean:
	rm -f ./bin/*
	rm -f ./internal/netobs/ebpf/netobs_bpfel.go ./internal/netobs/ebpf/netobs_bpfeb.go
	rm -f ./internal/netobs/ebpf/netobs_bpfel.o  ./internal/netobs/ebpf/netobs_bpfeb.o

tree:
	find . -maxdepth 4 -type f | sort
