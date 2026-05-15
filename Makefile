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
PREREQS_gpuobs-agent := generate-gpuobs

# go-nvml v0.13.x는 NVML 호출을 CGO `import "C"`로 구현해 CGO 비활성 빌드에서 심볼이
# 해석되지 않는다. gpuobs-agent만 CGO=1로, netobs-agent는 기존 정적 바이너리 속성을 유지한다.
CGO_netobs-agent := 0
CGO_gpuobs-agent := 1

# ============================================================================
# Overlay registry — 기본은 <agent-domain>-<rollout-stage> 형식이지만 dev/prod 분기가
# 없는 클러스터 공용 패키지는 단일 이름 (예: dashboards) 으로 등록한다.
# 새 overlay 추가 시:
#   1) OVERLAYS에 이름 추가
#   2) OVERLAY_PATH_<name>에 kustomize 경로 지정
# 이후 render-<name>, deploy-<name>, delete-<name>이 자동으로 매치된다.
# ============================================================================
OVERLAYS := netobs-dev netobs-prod gpuobs-dev gpuobs-prod dashboards

OVERLAY_PATH_netobs-dev  := deploy/netobs/overlays/dev
OVERLAY_PATH_netobs-prod := deploy/netobs/overlays/prod
OVERLAY_PATH_gpuobs-dev  := deploy/gpuobs/overlays/dev
OVERLAY_PATH_gpuobs-prod := deploy/gpuobs/overlays/prod
# dashboards 는 dev/prod 분기가 없는 클러스터 공용 패키지다. Grafana sidecar 가 cluster 전체
# ConfigMap 을 watch 하므로 단일 배포로 충분하다.
OVERLAY_PATH_dashboards  := deploy/dashboards

# ============================================================================
# Architecture detection (BPF 컴파일용)
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
.PHONY: deps generate generate-gpuobs clean tree bump \
	build-all image-build-all image-push-all \
	test test-integration setup-envtest \
	check-prometheus-rules

# ============================================================================
# Tests
# ----------------------------------------------------------------------------
# test           - 일반 단위 테스트 (race detector 항상 on, integration build tag 미포함)
# test-integration - envtest binary 를 setup-envtest 로 준비한 뒤 통합 테스트 실행 (#36)
# setup-envtest   - controller-runtime envtest binary 를 다운로드 / 캐싱하고 KUBEBUILDER_ASSETS
#                   경로를 stdout 으로 출력. 본 타깃은 test-integration 의 prerequisite 이며
#                   CI 가 캐시 키 산출 등에 직접 호출할 수도 있다.
# ============================================================================
ENVTEST_K8S_VERSION ?= 1.31.0
# setup-envtest 는 controller-runtime 본체에서 떨어져 나온 별도 Go 모듈
# (sigs.k8s.io/controller-runtime/tools/setup-envtest) 로 자체 태그 (v0.24.0~) 를 가진다. controller-runtime
# 본체 (v0.19.4) 와 태그 체계가 다르며 본체 태그로는 모듈 해상이 안 된다. 현재 발행된 단일 안정
# 태그인 v0.24.0 으로 고정해 시간 경과에 따른 재현성 흔들림 (자산 해석 동작 변경 / 업스트림 회귀)
# 을 차단한다. v0.24.0 의 go.mod 가 Go 1.26 을 요구하지만 modern `go run` 의 toolchain 자동 다운로드
# 로 1.22 host 에서도 정상 실행된다.
SETUP_ENVTEST_VERSION ?= v0.24.0

# test 는 단위 테스트 스위트. cuda / netobs ebpf 패키지가 //go:embed 로 bpf2go 산출물 (.o) 을 참조
# 하므로 fresh checkout 에서 컴파일 단계가 실패하지 않도록 generate / generate-gpuobs 를 선행 의존
# 으로 둔다. host 에 bpftool 이 없으면 generate 단계에서 명확한 오류로 즉시 멈춘다.
test: generate generate-gpuobs
	go test -race ./...

test-integration:
	@echo "preparing envtest binaries via setup-envtest (k8s $(ENVTEST_K8S_VERSION))"
	@KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test -tags=integration -race -timeout=60s \
		-ldflags='-extldflags "-Wl,-z,lazy"' \
		./internal/gpuobs/integration/...

# 통합 테스트 빌드시 host 의 gcc / ld 가 강제하는 BIND_NOW 플래그가 cuda 패키지의 go-nvml CGO PLT
# 엔트리를 binary load 시점에 strict 해상하려 해 libnvidia-ml.so 미존재 환경 (CI / 일반 dev 호스트)
# 에서 `undefined symbol: nvmlDeviceSetDriverModel` 류 오류로 startup 자체가 깨진다. lazy binding
# 으로 override 해 NVML 심볼은 runtime 의 dlopen / dlsym 경로로만 접근되도록 한다 (production 의
# go-nvml 도 dlopen 경로만 사용).

setup-envtest:
	@go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) -p path

# ============================================================================
# PrometheusRule lint
# ----------------------------------------------------------------------------
# check-prometheus-rules - deploy/gpuobs/base/prometheus-rule.yaml 의 PromQL 문법과 rule 정의 정합성
#                          을 promtool 로 검증한다. PrometheusRule CRD wrapper 를 그대로 promtool 에 줄
#                          수 없어 awk 로 spec.groups 만 추출해 promtool 입력 형식으로 변환한다. promtool
#                          은 공식 prom/prometheus 컨테이너에서 실행되어 호스트 설치를 요구하지 않으며
#                          PROMTOOL_IMAGE 변수로 버전을 pin 한다.
# ============================================================================
PROMTOOL_IMAGE ?= prom/prometheus:v2.55.0
PROMETHEUS_RULE_FILE ?= deploy/gpuobs/base/prometheus-rule.yaml

check-prometheus-rules:
	@echo "extracting spec.groups from $(PROMETHEUS_RULE_FILE)"
	@awk '/^spec:/{f=1;next} f' $(PROMETHEUS_RULE_FILE) | sed 's/^  //' > /tmp/promtool-rules.yaml
	@docker run --rm --entrypoint promtool -v /tmp/promtool-rules.yaml:/tmp/rules.yaml $(PROMTOOL_IMAGE) check rules /tmp/rules.yaml
	@echo "promtool check rules: OK"

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

# generate-gpuobs 는 gpuobs CUDA uprobe BPF 산출물을 internal/gpuobs/cuda 패키지로 emit 한다.
# vmlinux.h 갱신 + bpf2go 호출은 generate 와 동일한 패턴이며, 출력 prefix `CudaUprobe` 는
# 소문자화되어 cudauprobe_bpfel.{go,o} / cudauprobe_bpfeb.{go,o} 4 파일로 생성된다.
generate-gpuobs:
	@if [ -z "$(BPFTOOL)" ]; then echo "bpftool not found"; exit 1; fi
	@mkdir -p internal/gpuobs/cuda
	$(BPFTOOL) btf dump file /sys/kernel/btf/vmlinux format c > ./bpf/vmlinux.h && \
	cd internal/gpuobs/cuda && GOPACKAGE=cuda go run github.com/cilium/ebpf/cmd/bpf2go@v0.17.1 \
	-go-package cuda \
	-cc clang \
	-cflags "$(BPF_CFLAGS)" \
	CudaUprobe ../../../bpf/cuda_uprobe.bpf.c -- -I../../../bpf

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

image-build-%-agent: $$(PREREQS_$$*-agent)
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
	rm -f ./internal/gpuobs/cuda/cudauprobe_bpfel.go ./internal/gpuobs/cuda/cudauprobe_bpfeb.go
	rm -f ./internal/gpuobs/cuda/cudauprobe_bpfel.o  ./internal/gpuobs/cuda/cudauprobe_bpfeb.o

tree:
	find . -maxdepth 4 -type f | sort
