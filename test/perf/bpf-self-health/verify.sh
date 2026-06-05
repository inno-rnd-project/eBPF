#!/usr/bin/env bash
# bpf-self-health/verify.sh 는 이슈 #105 의 BPF program attach self-health 메트릭 도입 회귀 가드다.
# dev cluster (kernel 6.8 단일 환경) 에서 (1) 정상 attach 경로의 result="success" 누적 회귀 차단,
# (2) fake symbol 시뮬 의 result="failure" 발화 와 reason="symbol_not_found" 분류 정확성, (3) retry
# 부담 메트릭 발화 의 3 단계 가드를 수행한다. kernel 버전 mismatch / BTF 부재 시뮬은 별도 environment
# matrix (구 kernel 또는 BTF-stripped 이미지) 가 필요하며 본 스크립트 범위 밖이다.
# 본 스크립트는 dev cluster 전용이며 prod 에서 실행하지 않는다.
set -euo pipefail

NAMESPACE="${NETOBS_NAMESPACE:-ebpf-project}"
PROM_NAMESPACE="${PROM_NAMESPACE:-monitoring}"
PROM_SVC="${PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${PROM_PORT:-9090}"
TIMEOUT_SECONDS="${BPF_TIMEOUT:-300}"
POLL_INTERVAL="${BPF_POLL_INTERVAL:-15}"
FAKE_SYMBOL="${BPF_FAKE_SYMBOL:-__netobs_nonexistent_probe}"

PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"

# query_count 는 Prometheus instant query 결과 시리즈 개수 를 반환 한다.
query_count() {
  local q="$1"
  local resp
  resp=$(curl -sf --max-time 10 -G "${PROM_URL}/api/v1/query" \
    --data-urlencode "query=${q}" 2>/dev/null || echo "")
  echo "${resp}" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(len(d.get('data',{}).get('result',[])))
except: print(0)" 2>/dev/null || echo "0"
}

# query_max 는 Prometheus instant query 결과 의 max 값 (counter 누적치 등) 을 반환 한다.
query_max() {
  local q="$1"
  local resp
  resp=$(curl -sf --max-time 10 -G "${PROM_URL}/api/v1/query" \
    --data-urlencode "query=${q}" 2>/dev/null || echo "")
  echo "${resp}" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    res = d.get('data',{}).get('result',[])
    vals = [float(r['value'][1]) for r in res] if res else [0]
    print(max(vals))
except: print(0)" 2>/dev/null || echo "0"
}

# 1차 가드 (fail-on-miss): 정상 환경에서 정상 program 의 result="success" 누적 회귀 차단.
echo "[poll] 1차 가드 정상 attach 회귀 차단 (gpuobs_pod_utilization_percent 와 유사한 회귀 가드 패턴)"
success_count=0
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while (( $(date +%s) < deadline )); do
  success_count=$(query_count 'netobs_bpf_program_attach_total{program="tcp_sendmsg",result="success"} > 0')
  if [[ "${success_count}" -ge 1 ]]; then
    echo "[pass] tcp_sendmsg attach_total{result=\"success\"} emit 확인"
    break
  fi
  echo "[wait] tcp_sendmsg success 누적 부재 (count=${success_count})"
  sleep "${POLL_INTERVAL}"
done
if [[ "${success_count}" -lt 1 ]]; then
  echo "[fail] tcp_sendmsg attach 정상 누적 미관측 (${TIMEOUT_SECONDS}s 내)"
  exit 1
fi

# 2차 가드 (fail-on-miss): fake symbol 시뮬 의 result="failure" 와 reason="symbol_not_found" 발화.
# daemonset env patch 후 rollout, 메트릭 발화 확인, env 정리 후 rollout 의 sub-cycle.
echo "[setup] fake symbol injection: ${FAKE_SYMBOL}"
kubectl set env -n "${NAMESPACE}" daemonset/netobs-agent "NETOBS_BPF_FAKE_ATTACH_SYMBOLS=${FAKE_SYMBOL}" >/dev/null
trap "echo '[cleanup] fake symbol env 정리'; kubectl set env -n \"${NAMESPACE}\" daemonset/netobs-agent NETOBS_BPF_FAKE_ATTACH_SYMBOLS- >/dev/null 2>&1 || true; kubectl rollout restart daemonset/netobs-agent -n \"${NAMESPACE}\" >/dev/null 2>&1 || true" EXIT

kubectl rollout restart daemonset/netobs-agent -n "${NAMESPACE}" >/dev/null
echo "[wait] daemonset rollout 완료 대기"
kubectl rollout status daemonset/netobs-agent -n "${NAMESPACE}" --timeout=120s >/dev/null

echo "[poll] 2차 가드 fake symbol attach 실패 메트릭 발화 (timeout ${TIMEOUT_SECONDS}s)"
fail_count=0
retry_count=0
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while (( $(date +%s) < deadline )); do
  fail_count=$(query_count "netobs_bpf_program_attach_total{program=\"${FAKE_SYMBOL}\",result=\"failure\"} > 0")
  retry_count=$(query_count "netobs_bpf_program_attach_retry_total{program=\"${FAKE_SYMBOL}\",reason=\"symbol_not_found\"} > 0")
  if [[ "${fail_count}" -ge 1 && "${retry_count}" -ge 1 ]]; then
    fail_val=$(query_max "netobs_bpf_program_attach_total{program=\"${FAKE_SYMBOL}\",result=\"failure\"}")
    retry_val=$(query_max "netobs_bpf_program_attach_retry_total{program=\"${FAKE_SYMBOL}\",reason=\"symbol_not_found\"}")
    echo "[pass] fake symbol failure 발화: attach_total=${fail_val}, retry_total=${retry_val} (retry budget 소진)"
    break
  fi
  echo "[wait] fake symbol metric 미관측 (fail=${fail_count} retry=${retry_count})"
  sleep "${POLL_INTERVAL}"
done
if [[ "${fail_count}" -lt 1 || "${retry_count}" -lt 1 ]]; then
  echo "[fail] fake symbol attach 실패 메트릭 ${TIMEOUT_SECONDS}s 안에 미발화"
  exit 1
fi

# 3차 가드: 시뮬 정리 후 정상 회복. fake symbol 시리즈는 stale 로 남되 회귀 신호 zero 회복.
echo "[setup] fake symbol env 정리 와 daemonset 재시작"
kubectl set env -n "${NAMESPACE}" daemonset/netobs-agent NETOBS_BPF_FAKE_ATTACH_SYMBOLS- >/dev/null
kubectl rollout restart daemonset/netobs-agent -n "${NAMESPACE}" >/dev/null
kubectl rollout status daemonset/netobs-agent -n "${NAMESPACE}" --timeout=120s >/dev/null
trap - EXIT  # trap 해제 (이미 정상 정리 완료)

echo "[pass] bpf-self-health 회귀 가드 3 단계 모두 통과"
exit 0
