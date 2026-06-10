#!/usr/bin/env bash
# rca-e2e/verify.sh 는 이슈 #71 수용 조건 2번 의 자동화된 검증이다. dev cluster 에서 workload-
# injector cpu kind 부하를 인가해 GPUIdleWithCPUThrottle alert 을 발화시킨 뒤 rca-summarizer
# 의 /rca 응답 dominant_dimension 필드가 cpu 인지 확인한다.
#
# 본 스크립트는 dev cluster 전용이며 prod 에서 실행하지 않는다. 종료 시 injector Job 을 항상
# 정리한다 (메모리 규칙 feedback_gpu_bench_cleanup 준수).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${RCA_E2E_NAMESPACE:-ebpf-project}"
ALERT="${RCA_E2E_ALERT:-GPUIdleWithCPUThrottle}"
EXPECTED_DIM="${RCA_E2E_EXPECTED_DIM:-cpu}"
TIMEOUT_SECONDS="${RCA_E2E_TIMEOUT:-360}"
POLL_INTERVAL="${RCA_E2E_POLL_INTERVAL:-15}"

# rca-summarizer Service 의 ClusterIP 를 직접 호출한다. dev cluster 의 Service CIDR 가 host
# (검증 실행 환경) 에서 routable 한 환경 전제다. kubectl run 으로 임시 curl Pod 을 띄우는 패턴은
# 외부 image pull 가능 여부와 권한에 의존해 환경 휴대성이 떨어져 사용하지 않는다.
RCA_IP="${RCA_E2E_RCA_IP:-}"
if [[ -z "${RCA_IP}" ]]; then
  RCA_IP=$(kubectl get svc -n "${NAMESPACE}" rca-summarizer -o jsonpath='{.spec.clusterIP}')
fi
if [[ -z "${RCA_IP}" ]]; then
  echo "[fatal] failed to resolve rca-summarizer ClusterIP"
  exit 1
fi
RCA_URL="${RCA_E2E_RCA_URL:-http://${RCA_IP}:9850/rca}"
echo "[setup] rca-summarizer URL: ${RCA_URL}"

cleanup() {
  echo "[cleanup] deleting injector Job from namespace ${NAMESPACE}"
  kubectl delete -n "${NAMESPACE}" -f "${SCRIPT_DIR}/cpu-throttle.yaml" --ignore-not-found=true --wait=false || true
}
trap cleanup EXIT

echo "[setup] applying cpu injector to namespace ${NAMESPACE}"
kubectl apply -n "${NAMESPACE}" -f "${SCRIPT_DIR}/cpu-throttle.yaml"

echo "[poll] waiting up to ${TIMEOUT_SECONDS}s for ${ALERT} RCA summary"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
guard1_passed=0
guard1_response=""
while (( $(date +%s) < deadline )); do
  if response=$(curl -sf --max-time 10 "${RCA_URL}?alert=${ALERT}" 2>/dev/null); then
    if echo "${response}" | grep -q "\"dominant_dimension\":\"${EXPECTED_DIM}\""; then
      echo "[pass] ${ALERT} dominant_dimension=${EXPECTED_DIM}"
      echo "${response}"
      guard1_passed=1
      guard1_response="${response}"
      break
    fi
    echo "[wait] response received but dominant_dimension not yet ${EXPECTED_DIM}"
  else
    echo "[wait] no RCA summary yet for ${ALERT}"
  fi
  sleep "${POLL_INTERVAL}"
done

if (( guard1_passed == 0 )); then
  echo "[fail] timed out waiting for ${ALERT} RCA summary with dominant_dimension=${EXPECTED_DIM}"
  exit 1
fi

# 2차 가드 (#122): /rca 응답의 confidence_score 필드 존재 와 0-1 범위 검증. multi-source cross-
# reference 흐름 이 응답 schema 에 정합 하게 반영 되는지 회귀 차단. confidence 가 0 일 수도 있는
# (모든 source 가 빈 신호) 환경 에서도 필드 자체 는 emit 되어야 한다.
echo "[guard2] /rca response 의 confidence_score 필드 검증 (#122)"
confidence=$(echo "${guard1_response}" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    v = d.get('confidence_score')
    if v is None:
        print('MISSING')
    else:
        print(v)
except Exception as e:
    print('PARSE_ERROR:' + str(e))" 2>/dev/null || echo "PYTHON_FAILED")

if [[ "${confidence}" == "MISSING" ]]; then
  echo "[fail] /rca response 의 confidence_score 필드 부재. RCASummary schema 회귀 의심"
  exit 1
fi
if [[ "${confidence}" == PARSE_ERROR:* || "${confidence}" == "PYTHON_FAILED" ]]; then
  echo "[fail] /rca response 의 JSON parse 실패: ${confidence}"
  exit 1
fi
if ! awk "BEGIN { exit !(${confidence} >= 0 && ${confidence} <= 1) }"; then
  echo "[fail] confidence_score=${confidence} 가 0-1 범위 밖. clamp 회귀 의심"
  exit 1
fi
echo "[pass] confidence_score=${confidence} (0-1 범위 정합)"

# 3차 가드 (#122): rca-summarizer 의 /metrics 에서 rca_summary_confidence_score gauge 와 (선택적)
# rca_summary_skipped_total counter 의 시리즈 emit 확인. metric 자체 가 emit 되는지 회귀 차단 이며
# value 의 의미 검증 은 2차 가드 의 confidence_score 필드 가 cover 한다.
echo "[guard3] rca-summarizer /metrics 의 confidence_score gauge 회귀 차단 (#122)"
METRICS_URL="${RCA_E2E_METRICS_URL:-http://${RCA_IP}:9850/metrics}"
if ! metrics_resp=$(curl -sf --max-time 10 "${METRICS_URL}" 2>/dev/null); then
  echo "[fail] /metrics endpoint HTTP 응답 실패"
  exit 1
fi
if ! echo "${metrics_resp}" | grep -q "^rca_summary_confidence_score{"; then
  echo "[fail] /metrics 에 rca_summary_confidence_score gauge 시리즈 부재"
  exit 1
fi
echo "[pass] rca_summary_confidence_score gauge 시리즈 emit 정합"

exit 0
