#!/usr/bin/env bash
# protocol-coverage/verify.sh 는 이슈 #103 의 IPv6 와 UDP 추적 확장 회귀 가드 다. dev cluster 에서
# 신규 BPF kprobe 6 종 (tcp_v6_rcv, tcp_v6_do_rcv, udp_sendmsg, udp_recvmsg, udpv6_sendmsg,
# udpv6_recvmsg) 의 attach 성공 과 기존 IPv4 TCP 흐름 의 회귀 차단 을 확인 한다. flow_bytes 의
# 실 emit 은 NETOBS_FLOW_ALLOW_NAMESPACES 환경 변수 가 dev overlay 에 없 어 dev cluster 에서는
# 자연 비어 있으므로 본 가드 는 attach 성공 과 회귀 차단 만 검증 한다.
# 본 스크립트 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

NAMESPACE="${NETOBS_NAMESPACE:-ebpf-project}"
PROM_NAMESPACE="${PROM_NAMESPACE:-monitoring}"
PROM_SVC="${PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${PROM_PORT:-9090}"
TIMEOUT_SECONDS="${PROTO_TIMEOUT:-300}"
POLL_INTERVAL="${PROTO_POLL_INTERVAL:-15}"

PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"

# 1차 가드: 신규 6 kprobe 의 attach 성공 확인.
REQUIRED_SYMBOLS=(
  "tcp_v6_rcv"
  "tcp_v6_do_rcv"
  "udp_sendmsg"
  "udp_recvmsg"
  "udpv6_sendmsg"
  "udpv6_recvmsg"
)
echo "[poll] 1차 가드 신규 kprobe 6 종 attach 확인 (timeout ${TIMEOUT_SECONDS}s)"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while (( $(date +%s) < deadline )); do
  all_ok=1
  for sym in "${REQUIRED_SYMBOLS[@]}"; do
    resp=$(curl -sf --max-time 10 -G "${PROM_URL}/api/v1/query" \
      --data-urlencode "query=netobs_bpf_program_loaded{symbol=\"${sym}\"} == 1" 2>/dev/null || echo "")
    cnt=$(echo "${resp}" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(len(d.get('data',{}).get('result',[])))
except: print(0)" 2>/dev/null || echo "0")
    if [[ "${cnt}" -lt 1 ]]; then
      echo "[wait] ${sym} attach 미확인 (loaded=0 또는 scrape 미완)"
      all_ok=0
      break
    fi
  done
  if (( all_ok == 1 )); then
    echo "[pass] 신규 kprobe 6 종 모두 attach 성공"
    break
  fi
  sleep "${POLL_INTERVAL}"
done
if (( all_ok != 1 )); then
  echo "[fail] ${TIMEOUT_SECONDS}s 안에 6 kprobe attach 미완료"
  exit 1
fi

# 2차 가드: 기존 IPv4 TCP 흐름 회귀 차단. pod_bytes 시리즈 가 IPv4 TCP 트래픽 으로 갱신 되는지
# 확인. dev cluster 의 자연 트래픽 (Prometheus scrape, informer 등) 이 항상 있어 시리즈 0 이면 회귀.
echo "[poll] 2차 가드 IPv4 TCP 회귀 차단 (netobs_pod_bytes_total 시리즈 1 이상 emit)"
resp=$(curl -sf --max-time 10 -G "${PROM_URL}/api/v1/query" \
  --data-urlencode "query=count(netobs_pod_bytes_total)" 2>/dev/null || echo "")
cnt=$(echo "${resp}" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    res = d.get('data',{}).get('result',[])
    print(res[0]['value'][1] if res else 0)
except: print(0)" 2>/dev/null || echo "0")
if [[ "${cnt}" -lt 1 ]]; then
  echo "[fail] IPv4 TCP 회귀: netobs_pod_bytes_total 시리즈 0 (BPF struct 확장 후 누적 실패 의심)"
  exit 1
fi
echo "[pass] IPv4 TCP 회귀 차단: netobs_pod_bytes_total count=${cnt}"

# 3차 가드 (warn only): ip_version 라벨 의 cardinality 가 0 이 아닌 시리즈 가 있는지. flow_bytes 가
# allow-list 미설정 으로 비어 있으면 warn 으로 처리. count() 만 으로는 라벨 미부착 구버전 회귀 를
# 못 잡 으므로 ip_version=~"4|6" 매칭 query 로 명시 하여 라벨 존재 자체 를 가드 한다.
echo "[poll] 3차 가드 (warn only) ip_version 라벨 시리즈 존재"
resp=$(curl -sf --max-time 10 -G "${PROM_URL}/api/v1/query" \
  --data-urlencode "query=count(netobs_flow_bytes_total{ip_version=~\"4|6\"})" 2>/dev/null || echo "")
cnt=$(echo "${resp}" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    res = d.get('data',{}).get('result',[])
    print(res[0]['value'][1] if res else 0)
except: print(0)" 2>/dev/null || echo "0")
if [[ "${cnt}" -ge 1 ]]; then
  echo "[pass] netobs_flow_bytes_total count=${cnt} (ip_version 라벨 활성 가능)"
else
  echo "[warn] netobs_flow_bytes_total 시리즈 0. NETOBS_FLOW_ALLOW_NAMESPACES 환경 변수 미설정 으로 자연 비어 있음 (dev overlay 기본 동작)"
fi

echo "[pass] protocol-coverage 회귀 가드 1-2 단계 통과 (3 단계 warn 처리)"
exit 0
