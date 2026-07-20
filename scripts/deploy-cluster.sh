#!/usr/bin/env bash
# deploy-cluster.sh 는 #288 의 멀티클러스터 일괄 배포 스크립트다. 대상 kube context 와 클러스터
# overlay 이름을 인자로 받아 preflight 점검 → 의존 순서 배포 → rollout 과 Prometheus 채택 검증을
# 한 번에 수행한다. 개별 컴포넌트 재배포는 기존 make deploy-<comp>-<env> 를 그대로 쓰고, 본
# 스크립트는 새 클러스터 전체 기동과 fail-fast 사전 점검이 목적이다.
#
# 사용법: scripts/deploy-cluster.sh <kube-context> <overlay-이름> [--with-injector]
#   overlay 이름은 deploy/<comp>/overlays/<이름> 디렉토리 실존으로 검증한다 (dev, prod, 또는
#   온보딩 템플릿으로 복사한 클러스터별 overlay).
#   injector 는 부하 주입기라 dev 계열 클러스터 전용이며 --with-injector 로만 포함한다.
#
# preflight 는 조용한 누락을 fail-fast 한다. 특히 Prometheus Operator 의 serviceMonitorSelector /
# ruleSelector 의 release 라벨이 overlay 렌더 결과와 어긋나면 배포가 성공처럼 보여도 메트릭과
# rule 이 채택되지 않으므로 배포 전에 대조한다. GHCR pull secret (ghcr-creds) 과 GPU 노드 라벨은
# 비목표 규약대로 점검 (warn) 만 하고 생성하지 않는다.
#
# Prometheus 조회는 port-forward 대신 apiserver 서비스 프록시 (kubectl get --raw) 를 쓴다.
# port-forward 는 대상 노드에 socat 이 없으면 깨지는 실측 전례가 있고, 프록시는 추가 의존성이
# 없다. JSON 판정은 jq 의존을 피하려고 grep 기반으로 한다.
set -euo pipefail

usage() {
  echo "사용법: $0 <kube-context> <overlay-이름> [--with-injector]" >&2
  exit 2
}

CTX="${1:-}"
ENV_NAME="${2:-}"
WITH_INJECTOR=0
[ -n "$CTX" ] && [ -n "$ENV_NAME" ] || usage
shift 2
for arg in "$@"; do
  case "$arg" in
    --with-injector) WITH_INJECTOR=1 ;;
    *) usage ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

# 의존 순서: namespace 를 생성하는 netobs 가 먼저, 그 위에 gpuobs / correlation / rca-summarizer.
# dashboards 는 Grafana sidecar 의존의 클러스터 공용 패키지라 본 스크립트 밖이다 (make deploy-dashboards).
COMPONENTS=(netobs gpuobs correlation rca-summarizer)
if [ "$WITH_INJECTOR" = 1 ]; then
  COMPONENTS+=(injector)
fi

KC() { kubectl --context "$CTX" "$@"; }

info() { echo "[deploy-cluster] $*"; }
warn() { echo "[deploy-cluster][warn] $*" >&2; }
fail() { echo "[deploy-cluster][fail] $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------
info "preflight: context=$CTX overlay=$ENV_NAME"

# 0) _template 은 복사용 온보딩 템플릿 (#289) 이라 직접 배포를 거부한다.
[ "$ENV_NAME" != "_template" ] \
  || fail "_template 은 복사용 템플릿이다. docs/deploy/cluster-onboarding.md 절차대로 클러스터별 overlay 로 복사해 사용하라"

# 1) overlay 디렉토리 실존. 컴포넌트별 overlay 가 없으면 배포 자체가 불가라 가장 먼저 확인한다.
for comp in "${COMPONENTS[@]}"; do
  dir="deploy/$comp/overlays/$ENV_NAME"
  [ -d "$dir" ] || fail "overlay 없음: $dir (온보딩 템플릿으로 overlay 를 먼저 작성하라)"
done

# 2) context 연결.
KC get --raw /readyz >/dev/null 2>&1 || fail "context '$CTX' 로 API server 에 연결할 수 없다"

# 3) Prometheus Operator CRD. servicemonitors / prometheusrules 가 없으면 apply 자체가 실패하고,
#    prometheuses 가 없으면 아래 selector 대조 조회가 불가하다.
for crd in servicemonitors.monitoring.coreos.com prometheusrules.monitoring.coreos.com prometheuses.monitoring.coreos.com; do
  KC get crd "$crd" >/dev/null 2>&1 || fail "CRD $crd 부재 (Prometheus Operator 를 먼저 설치하라)"
done

# 4) release 라벨 대조. 기대값은 하드코딩하지 않고 gpuobs overlay 렌더 결과의 ServiceMonitor
#    release 라벨에서 추출한다. operator 의 두 selector 와 어긋나면 조용한 누락이라 fail-fast.
expected_release="$(kubectl kustomize "deploy/gpuobs/overlays/$ENV_NAME" \
  | awk '/^    release:/ {print $2; exit}')"
[ -n "$expected_release" ] || fail "렌더 결과에서 release 라벨을 찾지 못했다"
# 첫 Prometheus CR 의 selector 를 필드별로 개별 조회한다. 한 줄 공백 구분 파싱은 빈 필드에서
# 필드 밀림 (field shifting) 이 나므로 쓰지 않는다. 빈 selector ({}) 는 전체 채택이라 일치 검사
# 를 생략하고, selector 가 있는데 release 키가 overlay 와 다르면 조용한 누락이라 fail 한다.
read -r PROM_NS PROM_NAME <<<"$(KC get prometheus -A \
  -o jsonpath='{.items[0].metadata.namespace} {.items[0].metadata.name}')"
[ -n "${PROM_NS:-}" ] && [ -n "${PROM_NAME:-}" ] \
  || fail "Prometheus CR 이 없다 (kube-prometheus-stack 류 설치 확인)"
check_release() { # <selector 필드명>
  local sel release
  sel="$(KC get prometheus -n "$PROM_NS" "$PROM_NAME" -o jsonpath="{.spec.$1}")"
  if [ -z "$sel" ] || [ "$sel" = "{}" ]; then
    info "preflight: $1 이 비어 있어 (전체 채택) release 일치 검사를 생략한다"
    return 0
  fi
  release="$(KC get prometheus -n "$PROM_NS" "$PROM_NAME" -o jsonpath="{.spec.$1.matchLabels.release}")"
  [ "$release" = "$expected_release" ] \
    || fail "$1 ($sel) 이 overlay release '$expected_release' 와 다르다 (조용한 누락)"
}
check_release ruleSelector
check_release serviceMonitorSelector
info "preflight: release 라벨 일치 ($expected_release), Prometheus ns=$PROM_NS"

# 5) 점검 전용 (생성하지 않음): GHCR pull secret 과 GPU 노드 라벨. namespace 는 netobs 가
#    생성하므로 최초 배포에서는 secret 부재가 정상 경고다.
if ! KC get secret ghcr-creds -n ebpf-project >/dev/null 2>&1; then
  warn "ebpf-project/ghcr-creds pull secret 이 없다. 이미지 pull 이 실패하면 온보딩 문서대로 생성하라"
fi
# BSD wc 는 숫자 앞에 공백을 패딩하므로 tr 로 제거해 문자열 비교를 안정화한다.
gpu_nodes="$(KC get nodes -l accelerator=nvidia,observability.netobs/enabled=true -o name 2>/dev/null | wc -l | tr -d '[:space:]')"
if [ "$gpu_nodes" = 0 ]; then
  warn "accelerator=nvidia + observability.netobs/enabled=true 라벨 노드가 없어 gpuobs 가 스케줄되지 않는다"
fi

# ---------------------------------------------------------------------------
# deploy (의존 순서)
# ---------------------------------------------------------------------------
for comp in "${COMPONENTS[@]}"; do
  info "apply: $comp ($ENV_NAME)"
  KC apply -k "deploy/$comp/overlays/$ENV_NAME"
done

# ---------------------------------------------------------------------------
# verify: rollout → Prometheus target / rule 채택
# ---------------------------------------------------------------------------
rollout() { # kind/name, 존재할 때만 대기
  if KC get "$1" -n ebpf-project >/dev/null 2>&1; then
    KC rollout status "$1" -n ebpf-project --timeout=180s || fail "rollout 실패: $1"
  fi
}
rollout daemonset/netobs-agent
if [ "$gpu_nodes" != 0 ]; then
  rollout daemonset/gpuobs-agent
else
  info "verify: GPU 라벨 노드가 없어 gpuobs rollout 대기를 건너뛴다"
fi
rollout deployment/correlation-exporter
rollout deployment/rca-summarizer
if [ "$WITH_INJECTOR" = 1 ]; then
  rollout deployment/workload-injector-controller
fi

# Prometheus 채택. ServiceMonitor 반영은 scrape interval 을 타므로 90s 까지 폴링한다.
prom_api() { KC get --raw "/api/v1/namespaces/$PROM_NS/services/prometheus-operated:9090/proxy/api/v1/$1"; }
info "verify: Prometheus target/rule 채택 대기 (최대 90s)"
deadline=$(( $(date +%s) + 90 ))
while :; do
  targets_ok=0
  rules_ok=0
  if prom_api "targets?state=active" | grep -qE '"job"[[:space:]]*:[[:space:]]*"netobs-agent"'; then
    targets_ok=1
  fi
  if prom_api "rules?type=record" | grep -qE '"name"[[:space:]]*:[[:space:]]*"node:memory_pressure_score:5m"'; then
    rules_ok=1
  fi
  [ "$targets_ok" = 1 ] && [ "$rules_ok" = 1 ] && break
  [ "$(date +%s)" -lt "$deadline" ] || fail "Prometheus 채택 확인 실패 (targets_ok=$targets_ok rules_ok=$rules_ok). release 라벨과 ServiceMonitor 를 점검하라"
  sleep 10
done
info "verify: netobs-agent target 과 recording rule 채택 확인"

if [ "$gpu_nodes" != 0 ]; then
  prom_api "targets?state=active" | grep -qE '"job"[[:space:]]*:[[:space:]]*"gpuobs-agent"' \
    || warn "gpuobs-agent target 이 아직 없다 (scrape 대기 중이거나 ServiceMonitor 문제)"
fi

info "완료: $ENV_NAME overlay 전 컴포넌트 배포와 검증이 끝났다"
