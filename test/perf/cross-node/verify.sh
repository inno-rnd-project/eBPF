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

RENDERED_MANIFEST="$(mktemp -t cross-node-suspect-XXXXXX.yaml)"
cleanup() {
  echo "[cleanup] removing suspect-cpu Job from namespace ${NAMESPACE}"
  kubectl delete -n "${NAMESPACE}" -f "${RENDERED_MANIFEST}" --ignore-not-found=true --wait=false || true
  rm -f "${RENDERED_MANIFEST}"
}
trap cleanup EXIT

# correlation-exporter 의 CrossNodeEnabled 가 true 인지 사전 검증 한다. yaml 의 env block 은 name 과
# value 가 두 줄 로 나뉘어 grep -E 의 단일 라인 매칭 으로 잡을 수 없 으므로 jsonpath 로 env value
# 또는 args flag 를 정확히 추출 한다.
echo "[setup] checking correlation-exporter CrossNodeEnabled flag"
cross_env=$(kubectl get deploy -n "${NAMESPACE}" correlation-exporter \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CROSS_NODE")].value}' 2>/dev/null || true)
cross_arg=$(kubectl get deploy -n "${NAMESPACE}" correlation-exporter \
  -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' 2>/dev/null | grep -E '^--?cross-node' || true)
# correlation-exporter 의 main.go 가 "1" 과 "true" 둘 다 활성 값으로 수용 하므로 검증 도 양쪽을 통과
# 시킨다.
if [[ "${cross_env}" != "true" && "${cross_env}" != "1" && -z "${cross_arg}" ]]; then
  echo "[warn] correlation-exporter 에 CROSS_NODE=true/1 env 또는 --cross-node flag 가 설정 되어 있지 않다."
  echo "       opt-in 활성 후 재시도 하라: kubectl set env -n ${NAMESPACE} deploy/correlation-exporter CROSS_NODE=true"
fi

# suspect-cpu.yaml 의 TARGET_NODE 는 __CROSS_NODE_SUSPECT__ placeholder 다. verify.sh 가 SUSPECT_NODE
# env 값을 sed 로 치환해 dev cluster 마다 다른 노드명을 동일 manifest로 다룰 수 있게 한다.
echo "[setup] rendering suspect-cpu manifest with TARGET_NODE=${SUSPECT_NODE}"
sed "s|__CROSS_NODE_SUSPECT__|${SUSPECT_NODE}|g" "${SCRIPT_DIR}/suspect-cpu.yaml" > "${RENDERED_MANIFEST}"
echo "[setup] applying suspect-cpu Job to namespace ${NAMESPACE} (TARGET_NODE=${SUSPECT_NODE})"
kubectl apply -n "${NAMESPACE}" -f "${RENDERED_MANIFEST}"

# 1차 가드: victim_node == suspect_node 시리즈 가 0 개 인지 확인 한다. EnumerateNodePairs 의 핵심
# 정책 (동일 노드 페어 자동 제외) 회귀 가드 다. PromQL 은 label = label 비교 를 지원 하지 않 으므로
# correlation_cross_node_score 전체 시리즈 를 받아 python3 로 victim_node == suspect_node entry 를
# 세는 방식 으로 가드 한다.
echo "[guard] verifying no victim_node == suspect_node series exists"
self_loop_count=$(curl -sf --max-time 10 --data-urlencode 'query=correlation_cross_node_score' "${PROM_URL}/api/v1/query" 2>/dev/null | python3 -c "import json,sys
try:
    r=json.load(sys.stdin)['data']['result']
    print(sum(1 for x in r if x['metric'].get('victim_node')==x['metric'].get('suspect_node')))
except Exception:
    print(-1)" 2>/dev/null || echo "-1")
if [[ "${self_loop_count}" == "-1" ]]; then
  echo "[warn] self-loop 가드 의 prometheus query 실패. 시리즈 아직 미존재 일 수 있어 계속 진행 한다."
elif [[ "${self_loop_count}" -gt 0 ]]; then
  echo "[fail] victim_node == suspect_node 시리즈 가 ${self_loop_count} 개 존재 한다 (enumerate 가드 미동작)"
  exit 1
fi

# 2차 가드: 본 페어 의 score 가 threshold 이상 으로 도달 하는지 polling 한다.
echo "[poll] waiting up to ${TIMEOUT_SECONDS}s for cross_node_score{victim=${VICTIM_NODE},suspect=${SUSPECT_NODE},dimension=cpu} >= ${SCORE_THRESHOLD}"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
query="correlation_cross_node_score{victim_node=\"${VICTIM_NODE}\",suspect_node=\"${SUSPECT_NODE}\",dimension=\"cpu\"}"
while (( $(date +%s) < deadline )); do
  if response=$(curl -sf --max-time 10 --data-urlencode "query=${query}" "${PROM_URL}/api/v1/query" 2>/dev/null); then
    # grep 기반 추출은 공백 / 과학적 표기법 / NaN 등의 JSON 포맷 변형에 취약하므로 python3 의 json
    # 모듈로 첫 번째 result entry 의 value 를 안전 하게 추출 한다.
    value=$(echo "${response}" | python3 -c "import json,sys
try:
    res = json.load(sys.stdin)['data']['result']
    if res:
        print(res[0]['value'][1])
except Exception:
    pass" 2>/dev/null || echo "")
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
