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
RCA_URL="${RCA_E2E_RCA_URL:-http://rca-summarizer.ebpf-project.svc.cluster.local:9850/rca}"
TIMEOUT_SECONDS="${RCA_E2E_TIMEOUT:-360}"
POLL_INTERVAL="${RCA_E2E_POLL_INTERVAL:-15}"

cleanup() {
  echo "[cleanup] deleting injector Job"
  kubectl delete -f "${SCRIPT_DIR}/cpu-throttle.yaml" --ignore-not-found=true --wait=false || true
}
trap cleanup EXIT

echo "[setup] applying cpu injector"
kubectl apply -f "${SCRIPT_DIR}/cpu-throttle.yaml"

echo "[poll] waiting up to ${TIMEOUT_SECONDS}s for ${ALERT} RCA summary"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while (( $(date +%s) < deadline )); do
  # rca-summarizer 가 in-cluster ClusterIP 라 호스트에서 직접 curl 불가하다. 대신 prometheus
  # 클러스터 내 임시 Pod 또는 kubectl exec 으로 curl 한다. 가장 간단한 방법은 ClusterIP 가
  # routable 한 환경에서 kubectl run 으로 short-lived curl 컨테이너를 띄우는 것.
  if response=$(kubectl run rca-e2e-curl-$$ -n "${NAMESPACE}" --rm -i --restart=Never --quiet \
      --image=curlimages/curl:8.7.1 --command -- \
      curl -sf --max-time 10 "${RCA_URL}?alert=${ALERT}" 2>/dev/null); then
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
