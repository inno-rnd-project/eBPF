# syntax=docker/dockerfile:1.7

# ============================================================================
# Builder stage
# TARGET_AGENT ARG로 어느 에이전트를 빌드할지 선택한다. 기본값 netobs-agent는
# 인자 없이 `docker build .`를 실행했을 때 기존 동작을 보존하기 위한 fallback이며,
# Makefile의 image-build-<name>-agent 패턴 룰은 항상 --build-arg로 명시 전달한다.
# ============================================================================
# #421 base 이미지 digest 고정. 부동 태그는 같은 커밋이 시점에 따라 다른 이미지로 빌드되어
# 재현성이 없다. 갱신 절차: `docker buildx imagetools inspect <image>:<tag>` 의 Digest 로 아래
# 두 FROM 의 @sha256 을 교체하고 빌드 검증 후 커밋한다 (README 의 "Base image 갱신" 절 참조).
FROM golang:1.24-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac AS builder
ARG TARGETARCH=amd64
ARG TARGET_AGENT=netobs-agent
# CGO_ENABLED 기본값은 0(정적 바이너리). go-nvml처럼 CGO `import "C"` 경로로 구현된
# 의존성을 쓰는 에이전트(gpuobs-agent)는 Makefile의 CGO_<agent> 매핑에서 1을 전달한다.
ARG CGO_ENABLED=0

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY bpf ./bpf
COPY configs ./configs
# #102 의 LoadScenario CRD 타입 패키지. cmd/workload-injector 가 controller mode 에서 본 패키지를
# import 한다.
COPY api ./api

RUN CGO_ENABLED=${CGO_ENABLED} GOOS=linux GOARCH=${TARGETARCH} \
    go build -o /out/${TARGET_AGENT} ./cmd/${TARGET_AGENT}

# ============================================================================
# Runtime stage
# ============================================================================
# #421 distroless 전환 검토 결론: 채택 보류. gpuobs 의 NVML 이 host libnvidia-ml.so dlopen 에
# 의존해 glibc 동적 로더가 필요하고 (distroless/base 로는 가능하나 static 불가), 단일 Dockerfile
# 이 5 이미지를 공용하는 구조에서 셸 부재가 장애 시 컨테이너 내 진단 (readlink, ldd 등) 을 막아
# 이득 대비 위험이 크다. 대신 digest 고정과 non-root USER 로 이미지 측 방어를 확보한다.
FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

# Docker 문법상 ARG는 FROM 경계를 넘어 전파되지 않으므로 재선언이 필요하다.
# 기본값은 builder stage와 동일하게 유지해 drift를 차단한다.
ARG TARGET_AGENT=netobs-agent
# 에이전트 기본 포트. Makefile의 image-build-%-agent 패턴 룰이 PORT_<agent>
# 매핑에서 꺼내 --build-arg AGENT_PORT=<port>로 주입한다. 기본값 9810은
# `docker build .`만 실행했을 때 netobs-agent 기존 동작을 보존하기 위한 fallback이다.
ARG AGENT_PORT=9810
# #421 이미지 기본 실행 사용자. 기본 65532 (nonroot 관례) 로 두어 USER 미지정 root 실행이라는
# 이미지 측 공백을 없앤다. Deployment 3종 (correlation / rca / injector) 의 runAsUser 65532 와
# 정합하며, BPF 에이전트 2종 (netobs / gpuobs) 은 kubelet 이 부여하는 capabilities 가 root 프로세스
# 에만 effective 로 실리므로 Makefile 의 UID_<agent> 매핑이 0 을 명시 전달한다 (DaemonSet 은
# runAsUser 를 지정하지 않아 이미지 USER 가 그대로 적용된다).
ARG RUNTIME_UID=65532

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# 바이너리는 원래 이름을 그대로 루트에 두어 정체성을 보존한다.
# 예) netobs-agent 빌드 시 /netobs-agent, gpuobs-agent 빌드 시 /gpuobs-agent.
COPY --from=builder /out/${TARGET_AGENT} /${TARGET_AGENT}

# 안정 진입점 symlink. ENTRYPOINT가 TARGET_AGENT 값을 몰라도 동작하게 해
# 단일 Dockerfile이 모든 에이전트를 커버하도록 한다. 실제 바이너리는
# `readlink -f /agent`로 확인 가능하며 PID 1 신호 처리도 exec form으로 보존된다.
RUN ln -s /${TARGET_AGENT} /agent

# 이 이미지가 실제 사용하는 포트만 선언해 문서성을 정확히 유지한다. 실제 바인딩은
# K8s Pod spec의 ports 또는 `docker run -p`가 결정하며, EXPOSE는 이미지 inspect와
# `docker run -P` 자동 발행에만 영향을 준다.
EXPOSE ${AGENT_PORT}

USER ${RUNTIME_UID}

# ENTRYPOINT는 symlink를 고정 경로로 가리켜 에이전트에 독립적이다.
ENTRYPOINT ["/agent"]

# CMD는 의도적으로 빈 리스트로 둔다. 각 에이전트의 Go flag 기본값(config.Parse())이
# 단일 진실원이며, K8s Pod spec의 args:가 항상 이를 덮는다. Dockerfile CMD에 특정
# 에이전트용 기본 플래그를 고정하면 다른 에이전트 이미지에서 존재하지 않는 flag를
# 호출하는 사고가 발생하므로 이 자리에는 어떤 값도 두지 않는다.
CMD []
