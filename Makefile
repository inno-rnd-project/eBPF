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
OVERLAYS := netobs-dev netobs-prod gpuobs-dev gpuobs-prod correlation-dev correlation-prod injector-dev dashboards

OVERLAY_PATH_netobs-dev       := deploy/netobs/overlays/dev
OVERLAY_PATH_netobs-prod      := deploy/netobs/overlays/prod
OVERLAY_PATH_gpuobs-dev       := deploy/gpuobs/overlays/dev
OVERLAY_PATH_gpuobs-prod      := deploy/gpuobs/overlays/prod
OVERLAY_PATH_correlation-dev    := deploy/correlation/overlays/dev
OVERLAY_PATH_correlation-prod   := deploy/correlation/overlays/prod
OVERLAY_PATH_rca-summarizer-dev := deploy/rca-summarizer/overlays/dev
OVERLAY_PATH_rca-summarizer-prod := deploy/rca-summarizer/overlays/prod
# injector 는 본 시리즈 #52 의 비목표로 prod overlay 를 두지 않는다. dev / staging 한정.
OVERLAY_PATH_injector-dev     := deploy/injector/overlays/dev
# dashboards 는 dev/prod 분기가 없는 클러스터 공용 패키지다. Grafana sidecar 가 cluster 전체
# ConfigMap 을 watch 하므로 단일 배포로 충분하다.
OVERLAY_PATH_dashboards       := deploy/dashboards

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
.PHONY: deps generate generate-gpuobs generate-nccl clean tree bump \
	build-all image-build-all image-push-all \
	test test-integration setup-envtest \
	check-prometheus-rules \
	build-correlation-debug \
	build-correlation-exporter image-build-correlation-exporter image-push-correlation-exporter \
	build-workload-injector image-build-workload-injector image-push-workload-injector \
	build-rca-summarizer image-build-rca-summarizer image-push-rca-summarizer

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
# 로 1.24 host 에서도 정상 실행된다.
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
#                          수 없어 awk 로 spec.groups 만 추출해 promtool 입력 형식으로 변환한다. 임시
#                          산출물은 gitignore 대상인 ./bin 디렉토리에 두어 /tmp 공유 충돌 (병렬 CI, 다중
#                          사용자) 을 회피한다. promtool 은 공식 prom/prometheus 컨테이너에서 실행되어
#                          호스트 설치를 요구하지 않으며 PROMTOOL_IMAGE 변수로 버전을 pin 한다.
# ============================================================================
PROMTOOL_IMAGE ?= prom/prometheus:v2.55.0
PROMETHEUS_RULE_FILES ?= deploy/gpuobs/base/prometheus-rule.yaml deploy/correlation/base/prometheus-rule.yaml deploy/injector/base/prometheus-rule.yaml
PROMTOOL_RULES_TMP_DIR ?= bin/promtool-rules
# PROMTOOL_TEST_FILES 는 promtool test rules 로 실행할 unit test 파일이다. 각 test 파일은 gpuobs
# recording rule 을 입력 series 로부터 평가해 산출 series 의 기대값을 단정한다. rule_files 는 컨테이너
# 안에서 gpuobs.yaml (추출본) 을 가리킨다.
PROMTOOL_TEST_FILES ?= test/promtool/gpuobs-network-retrans.test.yaml test/promtool/gpuobs-new-cause-alerts.test.yaml

check-prometheus-rules:
	@mkdir -p $(PROMTOOL_RULES_TMP_DIR)
	@for f in $(PROMETHEUS_RULE_FILES); do \
		base=$$(basename $$(dirname $$(dirname $$f))); \
		out=$(PROMTOOL_RULES_TMP_DIR)/$$base.yaml; \
		echo "extracting spec.groups from $$f → $$out"; \
		awk '/^spec:/{f=1;next} f' $$f | sed 's/^  //' > $$out; \
		docker run --rm --entrypoint promtool -v $(CURDIR)/$$out:/tmp/rules.yaml $(PROMTOOL_IMAGE) check rules /tmp/rules.yaml || exit 1; \
	done
	@echo "promtool check rules: OK"

# test-prometheus-rules - gpuobs recording rule 의 동작을 promtool test rules 로 단정 검증한다.
#                         check-prometheus-rules 와 동일한 awk 추출로 spec.groups 만 뽑아 컨테이너의
#                         /tmp/gpuobs.yaml 로 마운트하고, test 파일은 rule_files: [gpuobs.yaml] 로 이를
#                         참조한다. #154 의 retrans → network_pressure → dominant cause 정합을 결정적
#                         으로 검증한다.
test-prometheus-rules:
	@mkdir -p $(PROMTOOL_RULES_TMP_DIR)
	@awk '/^spec:/{f=1;next} f' deploy/gpuobs/base/prometheus-rule.yaml | sed 's/^  //' > $(PROMTOOL_RULES_TMP_DIR)/gpuobs.yaml
	@for t in $(PROMTOOL_TEST_FILES); do \
		echo "promtool test rules $$t"; \
		docker run --rm --entrypoint promtool \
			-v $(CURDIR)/$(PROMTOOL_RULES_TMP_DIR)/gpuobs.yaml:/tmp/gpuobs.yaml \
			-v $(CURDIR)/$$t:/tmp/test.yaml \
			$(PROMTOOL_IMAGE) test rules /tmp/test.yaml || exit 1; \
	done
	@echo "promtool test rules: OK"

# ============================================================================
# Core utilities
# ============================================================================
deps:
	go mod tidy

# swag-init 은 #100 의 3 agent (correlation-exporter 와 netobs-agent 와 gpuobs-agent) 에 부착된
# swaggo 주석 으로부터 각 agent 의 swagger.json 과 swagger.yaml 을 생성 한다.
# internal/<agent>/api/docs/ 하위에 산출물 (go / json / yaml 3종) 이 떨어 진다. 호출 측은 빌드
# 전에 본 타겟 을 한 번 실행 해 OpenAPI 스펙 을 갱신 한다. --outputTypes 로 json 과 yaml 동시
# 생성을 강제 해 둘 사이의 drift 를 차단 한다. swag CLI 가 GOPATH/bin 에 설치 되어 있어야 한다
# (go install github.com/swaggo/swag/cmd/swag@latest).
SWAG ?= $(shell go env GOPATH)/bin/swag

# controller-gen 은 #102 의 LoadScenario CRD 신설에 사용한다. kubebuilder marker 가 붙은
# api/v1alpha1/*.go 로부터 CRD YAML (deploy/injector/base/<group>_<plural>.yaml, controller-gen
# 표준 컨벤션 으로 예: injector.netobs.io_loadscenarios.yaml) 과 DeepCopy 메서드
# (api/v1alpha1/zz_generated_deepcopy.go) 를 자동 생성한다. controller-runtime v0.19.x 와
# 호환되는 v0.16.x 계열 로 pin 해 marker 처리 동작이 흔들리지 않게 한다. controller-gen 은
# GOPATH/bin 에 설치 되어 있어야 한다 (go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)).
CONTROLLER_GEN_VERSION ?= v0.16.5
CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen

# controller-gen 바이너리 설치 보장 타깃. CI 와 로컬 dev 양쪽 에서 동일 흐름 으로 호출 가능 하다.
controller-gen-install:
	@if ! [ -x "$(CONTROLLER_GEN)" ] || ! "$(CONTROLLER_GEN)" --version 2>/dev/null | grep -q "$(CONTROLLER_GEN_VERSION)"; then \
		echo "installing controller-gen $(CONTROLLER_GEN_VERSION)"; \
		go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION); \
	fi
	@"$(CONTROLLER_GEN)" --version

# manifests 는 api/v1alpha1/ 의 kubebuilder marker 로부터 CRD YAML 을 생성 해
# deploy/injector/base/<group>_<plural>.yaml (controller-gen 표준 컨벤션) 에 떨어 뜨린다.
# crd:crdVersions=v1 옵션 으로 v1 CRD 단일 버전 만 출력 한다. api/v1alpha1/ 디렉토리 가 없으면
# controller-gen 은 silent 통과 한다.
manifests: controller-gen-install
	@if [ -d "api/v1alpha1" ]; then \
		"$(CONTROLLER_GEN)" \
			crd:crdVersions=v1 \
			paths="./api/v1alpha1/..." \
			output:crd:artifacts:config=deploy/injector/base; \
		echo "manifests generated under deploy/injector/base/ (<group>_<plural>.yaml convention)"; \
	else \
		echo "[skip] api/v1alpha1/ not present; manifests no-op"; \
	fi

# generate-crd 는 api/v1alpha1/ 의 kubebuilder marker 로부터 DeepCopy 메서드 (Go 객체 복사) 를
# api/v1alpha1/zz_generated_deepcopy.go 로 생성 한다. controller-runtime 의 client.Object 인터페이스
# 구현 에 필요 하다. 본 산출물 은 PR 에 함께 commit 되어야 한다 (gitignore 대상 아님).
generate-crd: controller-gen-install
	@if [ -d "api/v1alpha1" ]; then \
		"$(CONTROLLER_GEN)" \
			object:headerFile="hack/boilerplate.go.txt" \
			paths="./api/v1alpha1/..."; \
		echo "deepcopy generated: api/v1alpha1/zz_generated_deepcopy.go"; \
	else \
		echo "[skip] api/v1alpha1/ not present; generate-crd no-op"; \
	fi

swag-init:
	@if [ ! -x "$(SWAG)" ]; then echo "swag CLI 가 없습니다. go install github.com/swaggo/swag/cmd/swag@latest 를 실행하세요."; exit 1; fi
	# correlation 은 PodIdentity 같은 cross-package 타입 까지 OpenAPI schema 에 포함 시키려고
	# parseDependency 와 repo 루트 scope 채택. netobs 와 gpuobs 의 REST API 는 미사용 scaffold 라
	# #171 에서 제거 했고, REST API 는 소비처 (injector) 가 있는 correlation-exporter 만 유지 한다.
	$(SWAG) init -g cmd/correlation-exporter/main.go --parseDependency --outputTypes go,json,yaml --output internal/correlation/api/docs --instanceName correlation -d $(CURDIR)

# swag-merge 는 3 agent 의 개별 swagger.json (swag 가 생성하는 Swagger 2.0 형식) 을 python 으로
# 병합해 docs/api/openapi.yaml 단일 spec 으로 통합 한다. swag 산출물 의 spec version 을 유지 해
# definitions 키 를 그대로 보존한다. 자체 dashboard 의 로컬 import 용 fallback 산출물 이며 cluster
# 의 swagger-ui Pod 는 각 agent 의 /api/v1/swagger.json 을 dropdown 으로 직접 참조하기 때문에
# 본 통합본 의 필요성 은 보조 적이다. PyYAML 의존성 사전 체크 로 actionable 에러 메시지 제공.
swag-merge:
	@mkdir -p docs/api
	@if [ ! -f internal/correlation/api/docs/correlation_swagger.json ]; then echo "swag-init 을 먼저 실행 하세요"; exit 1; fi
	@python3 -c "import yaml" 2>/dev/null || (echo "PyYAML 이 필요합니다. pip3 install pyyaml 을 실행 하세요"; exit 1)
	@python3 -c "import json, yaml, glob; \
specs = [json.load(open(f)) for f in sorted(glob.glob('internal/*/api/docs/*_swagger.json'))]; \
merged = {'swagger': '2.0', 'info': {'title': 'netobs unified API', 'version': '$(VERSION)'}, 'paths': {}, 'definitions': {}}; \
[merged['paths'].update(s.get('paths', {})) for s in specs]; \
[merged['definitions'].update(s.get('definitions', {})) for s in specs]; \
yaml.safe_dump(merged, open('docs/api/openapi.yaml', 'w'), allow_unicode=True, sort_keys=False)"
	@echo "docs/api/openapi.yaml 생성 완료"

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

# generate-nccl 은 #134 의 NCCL collective uprobe BPF (bpf/nccl_uprobe.bpf.c) 로부터 bpf2go 산출물을
# internal/gpuobs/nccl 에 생성한다. generate-gpuobs 와 동일한 bpf2go 패턴이며 NCCL production
# Profiler (build tag nccl) 가 본 산출물을 //go:embed 로 참조한다. build tag nccl 비활성 기본 빌드는
# stub 의 noop 만 컴파일하므로 본 산출물을 참조하지 않는다.
generate-nccl:
	@if [ -z "$(BPFTOOL)" ]; then echo "bpftool not found"; exit 1; fi
	@mkdir -p internal/gpuobs/nccl
	$(BPFTOOL) btf dump file /sys/kernel/btf/vmlinux format c > ./bpf/vmlinux.h && \
	cd internal/gpuobs/nccl && GOPACKAGE=nccl go run github.com/cilium/ebpf/cmd/bpf2go@v0.17.1 \
	-go-package nccl \
	-cc clang \
	-cflags "$(BPF_CFLAGS)" \
	-tags nccl \
	NcclUprobe ../../../bpf/nccl_uprobe.bpf.c -- -I../../../bpf

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

# ============================================================================
# correlation-debug CLI
# ----------------------------------------------------------------------------
# build-correlation-debug - internal/correlation 라이브러리의 일회성 검증 CLI 를 빌드한다.
#                           DaemonSet / Deployment 형태로 cluster 에 배포되지 않으며 운영자가 로컬
#                           에서 build / 실행 (kubectl port-forward 로 Prometheus 접근) 한다.
#                           주기적 자동화는 correlation-exporter 가 담당한다.
# ============================================================================
build-correlation-debug:
	go fmt ./cmd/correlation-debug ./internal/correlation
	go build -o ./bin/correlation-debug ./cmd/correlation-debug

# ============================================================================
# correlation-exporter (Deployment binary)
# ----------------------------------------------------------------------------
# correlation-exporter 는 internal/correlation 라이브러리를 주기적으로 호출해 noisy neighbor 메트릭
# 을 Prometheus 로 노출한다. DaemonSet 인 agent 와 달리 cluster 단위 단일 Deployment 로 배치되며
# build-%-agent 패턴 룰의 -agent 접미사 컨벤션 밖에 있어 build-correlation-debug 와 동일하게 explicit
# 타깃으로 둔다. Dockerfile 은 TARGET_AGENT 빌드 arg 로 ./cmd/<name> 경로를 직접 가리키도록 generic
# 하게 작성되어 있어 별도 Dockerfile 을 두지 않고 root Dockerfile 을 그대로 reuse 한다.
# ============================================================================
CORRELATION_EXPORTER_PORT := 9830

build-correlation-exporter:
	go fmt ./cmd/correlation-exporter ./internal/correlation/exporter ./internal/correlation
	CGO_ENABLED=0 go build -o ./bin/correlation-exporter ./cmd/correlation-exporter

image-build-correlation-exporter:
	docker build \
		--build-arg TARGET_AGENT=correlation-exporter \
		--build-arg AGENT_PORT=$(CORRELATION_EXPORTER_PORT) \
		--build-arg CGO_ENABLED=0 \
		-t correlation-exporter:$(VERSION) .

image-push-correlation-exporter: image-build-correlation-exporter
	docker tag correlation-exporter:$(VERSION) $(REGISTRY_BASE)/correlation-exporter:$(VERSION)
	docker push $(REGISTRY_BASE)/correlation-exporter:$(VERSION)

# ============================================================================
# workload-injector (Job binary)
# ----------------------------------------------------------------------------
# workload-injector 는 dev / staging 환경에서 합성 부하를 트리거해 correlation 분석 layer 의 산출을
# 검증하는 단기 lifecycle Job 도구다. Deployment 가 아닌 Job 형태로 cluster 에 적용되며 build /
# image / push 흐름은 correlation-exporter 와 동일한 explicit 패턴을 따른다. PORT 9840 은 netobs
# 9810 / gpuobs 9820 / correlation-exporter 9830 의 다음 자연스러운 번호다.
# ============================================================================
WORKLOAD_INJECTOR_PORT := 9840

build-workload-injector:
	go fmt ./cmd/workload-injector ./internal/injector/...
	CGO_ENABLED=0 go build -o ./bin/workload-injector ./cmd/workload-injector

image-build-workload-injector:
	docker build \
		--build-arg TARGET_AGENT=workload-injector \
		--build-arg AGENT_PORT=$(WORKLOAD_INJECTOR_PORT) \
		--build-arg CGO_ENABLED=0 \
		-t workload-injector:$(VERSION) .

image-push-workload-injector: image-build-workload-injector
	docker tag workload-injector:$(VERSION) $(REGISTRY_BASE)/workload-injector:$(VERSION)
	docker push $(REGISTRY_BASE)/workload-injector:$(VERSION)

# ============================================================================
# rca-summarizer (Deployment binary)
# ----------------------------------------------------------------------------
# rca-summarizer 는 Alertmanager webhook 을 받아 발화 alert 의 root cause analysis 요약을 30 초
# 안에 산출해 /rca endpoint 로 노출한다. correlation-exporter 와 동일하게 cluster 단위 단일
# Deployment 로 배치되며 build 흐름은 explicit 타깃 패턴을 따른다. PORT 9850 은 9810 / 9820 /
# 9830 / 9840 의 다음 자연스러운 번호다.
# ============================================================================
RCA_SUMMARIZER_PORT := 9850

build-rca-summarizer:
	go fmt ./cmd/rca-summarizer ./internal/rca/...
	CGO_ENABLED=0 go build -o ./bin/rca-summarizer ./cmd/rca-summarizer

image-build-rca-summarizer:
	docker build \
		--build-arg TARGET_AGENT=rca-summarizer \
		--build-arg AGENT_PORT=$(RCA_SUMMARIZER_PORT) \
		--build-arg CGO_ENABLED=0 \
		-t rca-summarizer:$(VERSION) .

image-push-rca-summarizer: image-build-rca-summarizer
	docker tag rca-summarizer:$(VERSION) $(REGISTRY_BASE)/rca-summarizer:$(VERSION)
	docker push $(REGISTRY_BASE)/rca-summarizer:$(VERSION)

# 우산 타깃. AGENTS 리스트와 correlation-exporter, workload-injector, rca-summarizer 를 함께 일괄 처리한다.
build-all:       $(addprefix build-,$(AGENTS)) build-correlation-exporter build-workload-injector build-rca-summarizer
image-build-all: $(addprefix image-build-,$(AGENTS)) image-build-correlation-exporter image-build-workload-injector image-build-rca-summarizer
image-push-all:  $(addprefix image-push-,$(AGENTS)) image-push-correlation-exporter image-push-workload-injector image-push-rca-summarizer

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
#
# WARNING: bump가 모든 overlay의 newTag를 일괄 갱신하므로 bump 직후 곧바로 deploy-*를 실행하면
# 새 tag의 이미지가 registry에 없는 agent (본 PR에서 변경하지 않은 agent 포함) 가 ImagePullBackOff
# 상태로 떨어진다. 본 함정을 회피하려면 다음 두 패턴 중 하나를 따른다.
#
#   1) bump 직후 image-push-all을 먼저 수행 후 deploy-* 실행 (가장 안전, 권장)
#   2) PR에서 실제로 코드가 변경된 agent만 image-push-<name>으로 갱신하고 deploy-* 대상도 동일
#      agent로 한정 (다른 agent overlay도 tag가 갱신되어 있으므로 그 agent가 restart되는 순간
#      ImagePullBackOff 위험은 그대로 남는다. 단기 PR 검증 한정)
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
	echo "bumped $$CUR -> $$NEW"; \
	echo "[reminder] bump 직후 deploy-* 전에 image-push-all 또는 변경된 agent의 image-push-<name>을 실행하라"

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
