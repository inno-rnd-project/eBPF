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
while (( $(date +%s) < deadline )); do
  if response=$(curl -sf --max-time 10 "${RCA_URL}?alert=${ALERT}" 2>/dev/null); then
    if echo "${response}" | grep -q "\"dominant_dimension\":\"${EXPECTED_DIM}\""; then
      echo "[pass] ${ALERT} dominant_dimension=${EXPECTED_DIM}"
      echo "${response}"
      exit 0
    fi
    echo "[wait] response received but dominant_dimension not yet ${EXPECTED_DIM}"
  else
    echo "[wait] no RCA summary yet for ${ALERT}"
  fi
  sleep "${POLL_INTERVAL}"
done

echo "[fail] timed out waiting for ${ALERT} RCA summary with dominant_dimension=${EXPECTED_DIM}"
exit 1
