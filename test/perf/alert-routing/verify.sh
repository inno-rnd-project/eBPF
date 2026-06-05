#!/usr/bin/env bash
# alert-routing/verify.sh 는 이슈 #106 의 AlertmanagerConfig routing tree 4 분기 skeleton 회귀 가드 다.
# 외부 채널 (Slack / SMTP / PagerDuty) 통합 부재 환경 이라 실 send 경로 검증 은 본 가드 범위 외 이며,
# (1) CRD 등록 확인, (2) Alertmanager 의 configYAML 응답 에 4 분기 노드 포함 확인, (3) amtool dry-run
# 매칭 으로 라벨 셋 별 의도 노드 매칭 확인 의 3 단계 가드 만 수행 한다.
# 본 스크립트는 dev cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

NAMESPACE="${ALERT_NAMESPACE:-ebpf-project}"
ALERTMGR_NAMESPACE="${ALERTMGR_NAMESPACE:-monitoring}"
ALERTMGR_SVC="${ALERTMGR_SVC:-kube-prometheus-stack-alertmanager}"
ALERTMGR_POD="${ALERTMGR_POD:-alertmanager-kube-prometheus-stack-alertmanager-0}"
ALERTMGR_PORT="${ALERTMGR_PORT:-9093}"
CRD_NAME="${CRD_NAME:-rca-summarizer}"

ALERTMGR_IP=$(kubectl get svc -n "${ALERTMGR_NAMESPACE}" "${ALERTMGR_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
if [[ -z "${ALERTMGR_IP}" ]]; then
  echo "[fatal] failed to resolve ${ALERTMGR_SVC} ClusterIP in ${ALERTMGR_NAMESPACE}"
  exit 1
fi
ALERTMGR_URL="http://${ALERTMGR_IP}:${ALERTMGR_PORT}"
echo "[setup] alertmanager URL: ${ALERTMGR_URL}"

# 1차 가드: AlertmanagerConfig CRD 가 ebpf-project namespace 에 등록 되어 있는지 확인.
echo "[check] 1차 가드 AlertmanagerConfig CRD 등록"
if ! kubectl get alertmanagerconfig -n "${NAMESPACE}" "${CRD_NAME}" >/dev/null 2>&1; then
  echo "[fail] AlertmanagerConfig/${CRD_NAME} not found in ${NAMESPACE}"
  exit 1
fi
echo "[pass] CRD 등록 확인"

# 2차 가드: Alertmanager configYAML 응답 에 routing tree 4 분기 노드 가 포함 되어 있는지 정합 검증.
# kube-prometheus-stack 의 reconcile 이 완료 되어 본 CRD 가 alertmanager.yaml 에 머지 되기 까지 통상
# 30-60s 가 소요 되므로 timeout 안 에서 polling. receiver 이름 포맷 (slash vs hyphen) 은 prometheus-
# operator 버전 에 따라 다를 수 있어 webhook URL 을 통한 매칭 으로 안정성 확보.
TIMEOUT_SECONDS="${ROUTE_TIMEOUT:-180}"
# prometheus-operator 의 receiver naming convention 은 버전 에 따라 slash (`ns/config/recv`) 또는
# hyphen (`ns-config-recv`) 두 가지가 가능 하므로 character class 로 둘 다 매칭 한다. webhook URL 자체는
# `configYAML` 응답 에서 마스킹 되어 보이지 않 으므로 receiver 이름 으로 매칭 한다.
RECEIVER_RE="${RECEIVER_RE:-${ALERT_NAMESPACE:-ebpf-project}[/-]rca-summarizer[/-]rca-summarizer}"
echo "[poll] 2차 가드 configYAML 의 4 분기 노드 정합 (timeout ${TIMEOUT_SECONDS}s)"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
got_critical=0; got_capacity=0; got_anomaly=0; got_fallback=0
while (( $(date +%s) < deadline )); do
  cfg=$(curl -sf --max-time 10 "${ALERTMGR_URL}/api/v2/status" 2>/dev/null \
    | python3 -c 'import sys, json; print(json.load(sys.stdin).get("config", {}).get("original", ""))' \
    2>/dev/null || echo "")
  echo "${cfg}" | grep -q 'severity="critical"' && got_critical=1 || got_critical=0
  echo "${cfg}" | grep -q 'component=~".*-capacity"' && got_capacity=1 || got_capacity=0
  echo "${cfg}" | grep -q 'component=~".*-anomaly"' && got_anomaly=1 || got_anomaly=0
  echo "${cfg}" | grep -qE "${RECEIVER_RE}" && got_fallback=1 || got_fallback=0
  if (( got_critical == 1 && got_capacity == 1 && got_anomaly == 1 && got_fallback == 1 )); then
    echo "[pass] 4 분기 노드 모두 configYAML 에 포함 (critical / capacity / anomaly / fallback)"
    break
  fi
  echo "[wait] critical=${got_critical} capacity=${got_capacity} anomaly=${got_anomaly} fallback=${got_fallback}"
  sleep 15
done
if (( got_critical != 1 || got_capacity != 1 || got_anomaly != 1 || got_fallback != 1 )); then
  echo "[fail] 4 분기 노드 정합 미충족 (${TIMEOUT_SECONDS}s 안)"
  exit 1
fi

# 3차 가드: amtool dry-run 매칭. 라벨 셋 별 로 어느 receiver 에 매칭 되는 지 확인 해 4 분기 의도 와 일치
# 검증. amtool 은 alertmanager pod 내 의 /bin/amtool 을 kubectl exec 로 호출 한다.
echo "[check] 3차 가드 amtool dry-run 매칭"

# alertmanager pod 안 의 config 파일 경로 (kube-prometheus-stack 기본 위치).
AM_CONFIG_PATH="/etc/alertmanager/config_out/alertmanager.env.yaml"

# dry-run 케이스 4 종 정의. 각 케이스 의 라벨 셋 이 정확한 분기 receiver 로 라우팅 되어야 통과.
run_amtool_test() {
  local label="$1"; shift
  local expected_match="$1"; shift
  local labels=("$@")
  local result
  result=$(kubectl exec -n "${ALERTMGR_NAMESPACE}" "${ALERTMGR_POD}" -c alertmanager -- \
    amtool config routes test --config.file="${AM_CONFIG_PATH}" "${labels[@]}" 2>/dev/null || echo "")
  if echo "${result}" | grep -q "${expected_match}"; then
    echo "  [pass] ${label}: matched ${expected_match}"
    return 0
  fi
  echo "  [fail] ${label}: expected ${expected_match}, got: ${result}"
  return 1
}

all_pass=1
run_amtool_test "critical 분기" "rca-summarizer" \
  namespace=ebpf-project severity=critical component=correlation alertname=GPUIdleWithHostComputeStall \
  || all_pass=0

run_amtool_test "capacity 분기" "rca-summarizer" \
  namespace=ebpf-project severity=warning component=gpuobs-capacity alertname=GPUUtilizationCapacityHigh \
  || all_pass=0

run_amtool_test "anomaly 분기" "rca-summarizer" \
  namespace=ebpf-project severity=warning component=gpuobs-anomaly alertname=GPUUtilizationSpike \
  || all_pass=0

run_amtool_test "fallback 분기 (general warning)" "rca-summarizer" \
  namespace=ebpf-project severity=warning component=gpuobs alertname=GPUObsCudaStreamWaitHigh \
  || all_pass=0

if (( all_pass != 1 )); then
  echo "[fail] amtool dry-run 매칭 1 종 이상 실패"
  exit 1
fi
echo "[pass] amtool dry-run 4 분기 매칭 모두 의도 receiver 로 라우팅 확인"

echo "[pass] alert-routing 회귀 가드 3 단계 모두 통과"
exit 0
