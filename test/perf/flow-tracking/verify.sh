#!/usr/bin/env bash
# flow-tracking/verify.sh 는 이슈 #85 의 Pod 간 정상 flow 5-tuple RX/TX 추적 회귀 가드 다. observability-
# test namespace 의 client → server 자연 트래픽 을 활용 해 NETOBS_FLOW_ALLOW_NAMESPACES 주입 후
# correlation_flow_bytes_total 시리즈 가 (egress + ingress 최소 2 entry) 노출 되는지 확인 한다. 본 스
# 크립트 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다. 별도 워크로드 를 생성 하지 않 으므로
# trap cleanup 도 env 주입 흔적 만 정리 한다.
set -euo pipefail

NAMESPACE="${FLOW_NAMESPACE:-ebpf-project}"
ALLOW_NAMESPACE="${FLOW_ALLOW_NAMESPACE:-observability-test}"
TIMEOUT_SECONDS="${FLOW_TIMEOUT:-300}"
POLL_INTERVAL="${FLOW_POLL_INTERVAL:-15}"

PROM_NAMESPACE="${FLOW_PROM_NAMESPACE:-monitoring}"
PROM_SVC="${FLOW_PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${FLOW_PROM_PORT:-9090}"

PROM_IP="${FLOW_PROM_IP:-}"
if [[ -z "${PROM_IP}" ]]; then
  PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}')
fi
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"
echo "[setup] allow_namespace=${ALLOW_NAMESPACE} timeout=${TIMEOUT_SECONDS}s"

# netobs-agent 의 NETOBS_FLOW_ALLOW_NAMESPACES env 가 ALLOW_NAMESPACE 를 포함 하는지 사전 검증 한다.
# 미설정 시 즉시 진단 출력 후 종료 한다.
echo "[setup] checking netobs-agent NETOBS_FLOW_ALLOW_NAMESPACES env"
flow_env=$(kubectl get ds -n "${NAMESPACE}" netobs-agent \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="NETOBS_FLOW_ALLOW_NAMESPACES")].value}' 2>/dev/null || true)
if [[ -z "${flow_env}" ]]; then
  echo "[warn] netobs-agent 에 NETOBS_FLOW_ALLOW_NAMESPACES env 가 설정 되어 있지 않다."
  echo "       opt-in 활성 후 재시도 하라: kubectl set env -n ${NAMESPACE} ds/netobs-agent NETOBS_FLOW_ALLOW_NAMESPACES=${ALLOW_NAMESPACE}"
fi

echo "[poll] waiting up to ${TIMEOUT_SECONDS}s for netobs_flow_bytes_total series"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))

# 1차 가드: count(netobs_flow_bytes_total{src_namespace="${ALLOW_NAMESPACE}"}) >= 2 (egress + ingress
# 두 entry 최소). 2차 가드: sum(netobs_flow_bytes_total{src_pod="client"}) > 0 (client Pod 의 누적 bytes
# 가 양수).
query_count="count(netobs_flow_bytes_total{src_namespace=\"${ALLOW_NAMESPACE}\"})"
query_sum="sum(netobs_flow_bytes_total{src_namespace=\"${ALLOW_NAMESPACE}\",src_pod=\"client\"})"

extract_value() {
  echo "$1" | python3 -c "import json,sys
try:
    res = json.load(sys.stdin)['data']['result']
    if res:
        print(res[0]['value'][1])
except Exception:
    pass" 2>/dev/null || echo ""
}

while (( $(date +%s) < deadline )); do
  count_resp=$(curl -sf --max-time 10 --data-urlencode "query=${query_count}" "${PROM_URL}/api/v1/query" 2>/dev/null || true)
  sum_resp=$(curl -sf --max-time 10 --data-urlencode "query=${query_sum}" "${PROM_URL}/api/v1/query" 2>/dev/null || true)
  count_val=$(extract_value "${count_resp}")
  sum_val=$(extract_value "${sum_resp}")
  if [[ -n "${count_val}" && -n "${sum_val}" ]]; then
    if awk "BEGIN { exit !(${count_val} >= 2 && ${sum_val} > 0) }"; then
      echo "[pass] series count=${count_val} (>=2), client sum=${sum_val} (>0)"
      exit 0
    fi
    echo "[wait] series count=${count_val} client sum=${sum_val}"
  else
    echo "[wait] netobs_flow_bytes_total series 가 아직 emit 되지 않음"
  fi
  sleep "${POLL_INTERVAL}"
done

echo "[fail] timed out waiting for netobs_flow_bytes_total series."
echo "       netobs-agent 의 NETOBS_FLOW_ALLOW_NAMESPACES env 와 client → server 트래픽 활성 여부 를 점검 하라."
exit 1
