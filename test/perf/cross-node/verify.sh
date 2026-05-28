#!/usr/bin/env bash
# cross-node/verify.sh 는 이슈 #84 의 cross-node interference layer 회귀 가드 다. 한 노드 (suspect_
# node, default ebpf-worker1) 에 workload-injector cpu Kind 부하 를 인가 하고 다른 노드 (victim_node,
# default gpu) 에 위치 한 latency-sensitive workload 사이 의 correlation_cross_node_score 가 임계
# 이상 으로 산정 되는지 확인 한다. 동시에 victim_node == suspect_node 시리즈 가 0 개 임을 회귀 가드
# 한다.
#
# 본 스크립트 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다. 종료 시 stress Job 을 항상 정리
# 한다 (메모리 규칙 feedback_gpu_bench_cleanup 준수).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${CROSS_NODE_NAMESPACE:-ebpf-project}"
VICTIM_NODE="${CROSS_NODE_VICTIM:-gpu}"
SUSPECT_NODE="${CROSS_NODE_SUSPECT:-ebpf-worker1}"
SCORE_THRESHOLD="${CROSS_NODE_THRESHOLD:-0.3}"
TIMEOUT_SECONDS="${CROSS_NODE_TIMEOUT:-540}"
POLL_INTERVAL="${CROSS_NODE_POLL_INTERVAL:-15}"

PROM_NAMESPACE="${CROSS_NODE_PROM_NAMESPACE:-monitoring}"
PROM_SVC="${CROSS_NODE_PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${CROSS_NODE_PROM_PORT:-9090}"

PROM_IP="${CROSS_NODE_PROM_IP:-}"
if [[ -z "${PROM_IP}" ]]; then
  PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}')
fi
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"
echo "[setup] victim_node=${VICTIM_NODE} suspect_node=${SUSPECT_NODE} threshold=${SCORE_THRESHOLD}"

cleanup() {
  echo "[cleanup] removing suspect-cpu Job from namespace ${NAMESPACE}"
  kubectl delete -n "${NAMESPACE}" -f "${SCRIPT_DIR}/suspect-cpu.yaml" --ignore-not-found=true --wait=false || true
}
trap cleanup EXIT

# correlation-exporter 의 CrossNodeEnabled 가 true 인지 사전 검증 한다. 미설정 시 series 자체 가
# emit 되지 않 으므로 본 단계 에서 즉시 진단 출력 후 종료 한다.
echo "[setup] checking correlation-exporter CrossNodeEnabled flag"
if ! kubectl get deploy -n "${NAMESPACE}" correlation-exporter -o yaml 2>/dev/null | grep -qE "CROSS_NODE.*true|--cross-node"; then
  echo "[warn] correlation-exporter 에 CROSS_NODE=true env 또는 --cross-node flag 가 보이지 않는다."
  echo "       opt-in 활성 후 재시도 하라: kubectl set env -n ${NAMESPACE} deploy/correlation-exporter CROSS_NODE=true"
fi

echo "[setup] applying suspect-cpu Job to namespace ${NAMESPACE} (TARGET_NODE=${SUSPECT_NODE})"
kubectl apply -n "${NAMESPACE}" -f "${SCRIPT_DIR}/suspect-cpu.yaml"

# 1차 가드: victim_node == suspect_node 시리즈 가 0 개 인지 확인 한다. EnumerateNodePairs 의 핵심
# 정책 (동일 노드 페어 자동 제외) 회귀 가드 다.
echo "[guard] verifying no victim_node == suspect_node series exists"
self_loop=$(curl -sf --max-time 10 --data-urlencode 'query=count(correlation_cross_node_score{victim_node=suspect_node})' "${PROM_URL}/api/v1/query" 2>/dev/null || true)
if echo "${self_loop}" | grep -qE '"value":\[[0-9.]+,"[1-9]'; then
  echo "[fail] victim_node == suspect_node 시리즈 가 존재 한다 (enumerate 가드 미동작)"
  echo "       ${self_loop}"
  exit 1
fi

# 2차 가드: 본 페어 의 score 가 threshold 이상 으로 도달 하는지 polling 한다.
echo "[poll] waiting up to ${TIMEOUT_SECONDS}s for cross_node_score{victim=${VICTIM_NODE},suspect=${SUSPECT_NODE},dimension=cpu} >= ${SCORE_THRESHOLD}"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
query="correlation_cross_node_score{victim_node=\"${VICTIM_NODE}\",suspect_node=\"${SUSPECT_NODE}\",dimension=\"cpu\"}"
while (( $(date +%s) < deadline )); do
  if response=$(curl -sf --max-time 10 --data-urlencode "query=${query}" "${PROM_URL}/api/v1/query" 2>/dev/null); then
    value=$(echo "${response}" | grep -oE '"value":\[[0-9.]+,"[0-9.]+"\]' | grep -oE '"[0-9.]+"\]' | grep -oE '[0-9.]+' | head -1)
    if [[ -n "${value}" ]]; then
      if awk "BEGIN { exit !(${value} >= ${SCORE_THRESHOLD}) }"; then
        echo "[pass] cross_node_score=${value} >= ${SCORE_THRESHOLD}"
        echo "${response}"
        exit 0
      fi
      echo "[wait] cross_node_score=${value} (< ${SCORE_THRESHOLD})"
    else
      echo "[wait] cross_node_score series 가 아직 emit 되지 않음"
    fi
  else
    echo "[wait] prometheus query 실패"
  fi
  sleep "${POLL_INTERVAL}"
done

echo "[fail] timed out waiting for cross_node_score{victim=${VICTIM_NODE},suspect=${SUSPECT_NODE},dimension=cpu} >= ${SCORE_THRESHOLD}"
echo "       correlation-exporter 의 CrossNodeEnabled 와 DefaultMetrics 의 node-level series 노출 여부 를 점검 하라."
exit 1
