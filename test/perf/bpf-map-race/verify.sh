#!/usr/bin/env bash
# bpf-map-race/verify.sh 는 이슈 #107 의 BPF map race condition audit 회귀 가드 다. dev cluster 의
# multi-pod / multi-CPU 환경 에서 의도적 race 자극 시나리오 로 BPF map 의 일관성 정합 을 검증 한다.
# 비결정적 timing 의존 이라 (1) 1차 가드 multi-stream 트래픽 counter monotonic 은 fail-on-miss, (2)
# 2차 가드 rapid Pod lifecycle 의 map utilization 정합 은 warn-only, (3) 3차 가드 drop event
# divergence 분석 은 warn-only 로 분리.
# 본 스크립트는 dev cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

NAMESPACE="${NETOBS_NAMESPACE:-ebpf-project}"
PROM_NAMESPACE="${PROM_NAMESPACE:-monitoring}"
PROM_SVC="${PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${PROM_PORT:-9090}"
TRAFFIC_DURATION="${TRAFFIC_DURATION:-90}"
POLL_INTERVAL="${POLL_INTERVAL:-10}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
IPERF_YAML="${REPO_ROOT}/test/perf/bpf-map-race/iperf3-multistream.yaml"

PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"

# Prometheus 의 /-/ready 헬스 체크 로 endpoint 도달 가능성 사전 검증. 도달 불가 시 query_value 의
# `|| echo "0"` 폴백 으로 모든 메트릭 조회 결과 가 "0" 이 되어 monotonic 검증 (0.0 >= 0.0) 이 항상
# pass 하는 false positive 결함 회피.
if ! curl -sf --max-time 5 "${PROM_URL}/-/ready" >/dev/null 2>&1; then
  echo "[fatal] Prometheus 가 ${PROM_URL} 에서 도달 불가. ClusterIP 라 본 스크립트 는 클러스터 내부 에서 실행 필요"
  exit 1
fi
echo "[ok] prometheus /-/ready 응답 확인"

query_value() {
  local q="$1"
  curl -sf --max-time 10 -G "${PROM_URL}/api/v1/query" --data-urlencode "query=${q}" 2>/dev/null \
    | python3 -c 'import sys, json; r=json.load(sys.stdin).get("data",{}).get("result",[]); print(r[0]["value"][1] if r else 0)' \
    2>/dev/null || echo "0"
}

# 1차 가드 (fail-on-miss): multi-stream 트래픽 발생 후 counter monotonic 증가 검증.
# race 발생 시 일시적 감소 또는 stale read 의 signature 가 본 가드 에 잡힌다.
echo "[setup] iperf3 multi-stream 워크로드 적용 (-P 16)"
kubectl apply -f "${IPERF_YAML}" >/dev/null
trap "echo '[cleanup] iperf3 워크로드 정리'; kubectl delete -f \"${IPERF_YAML}\" --ignore-not-found >/dev/null 2>&1 || true" EXIT

echo "[wait] iperf3-server / iperf3-client Pod Running 대기"
deadline=$(($(date +%s) + 120))
while [ $(date +%s) -lt $deadline ]; do
  server_phase=$(kubectl get pod -n "${NAMESPACE}" iperf3-server -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
  client_phase=$(kubectl get pod -n "${NAMESPACE}" iperf3-client -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
  if [[ "${server_phase}" == "Running" && "${client_phase}" == "Running" ]]; then
    echo "[ok] iperf3 Pods Running"
    break
  fi
  sleep 5
done

echo "[poll] 1차 가드 counter monotonic 증가 검증 (${TRAFFIC_DURATION}s 모니터링)"
# baseline 수집 후 N초 대기, 그 사이 monotonic 증가 정합 확인.
baseline_flow=$(query_value 'sum(netobs_flow_bytes_total)')
baseline_pod=$(query_value 'sum(netobs_pod_bytes_total)')
echo "  baseline: flow_bytes=${baseline_flow} pod_bytes=${baseline_pod}"

monotonic_violation=0
prev_flow="${baseline_flow}"
prev_pod="${baseline_pod}"
end_at=$(($(date +%s) + TRAFFIC_DURATION))
while [ $(date +%s) -lt $end_at ]; do
  sleep "${POLL_INTERVAL}"
  cur_flow=$(query_value 'sum(netobs_flow_bytes_total)')
  cur_pod=$(query_value 'sum(netobs_pod_bytes_total)')
  # python 의 float 비교 로 보수적 검증 (>= 로 정합).
  flow_ok=$(python3 -c "print(1 if float('${cur_flow}') >= float('${prev_flow}') else 0)")
  pod_ok=$(python3 -c "print(1 if float('${cur_pod}') >= float('${prev_pod}') else 0)")
  if [[ "${flow_ok}" != "1" || "${pod_ok}" != "1" ]]; then
    echo "  [violation] flow ${prev_flow} -> ${cur_flow}, pod ${prev_pod} -> ${cur_pod}"
    monotonic_violation=1
  fi
  echo "  tick: flow=${cur_flow} pod=${cur_pod}"
  prev_flow="${cur_flow}"
  prev_pod="${cur_pod}"
done

if (( monotonic_violation == 1 )); then
  echo "[fail] counter monotonic 위반 감지 (race 결함 의심)"
  exit 1
fi

# 트래픽 발생 후 baseline 대비 의미 있는 증가 가 있어야 race 가드 의 유의미성 확보.
final_flow=$(query_value 'sum(netobs_flow_bytes_total)')
final_pod=$(query_value 'sum(netobs_pod_bytes_total)')
flow_grew=$(python3 -c "print(1 if float('${final_flow}') > float('${baseline_flow}') else 0)")
pod_grew=$(python3 -c "print(1 if float('${final_pod}') > float('${baseline_pod}') else 0)")
if [[ "${flow_grew}" != "1" || "${pod_grew}" != "1" ]]; then
  echo "[warn] 트래픽 발생 후 counter 증가 미관측 (flow=${baseline_flow}->${final_flow}, pod=${baseline_pod}->${final_pod})"
  echo "[warn] flow_bytes 는 NETOBS_FLOW_ALLOW_NAMESPACES 가 dev overlay 에 없어 자연 0 일 수 있음 (정상)"
else
  echo "[pass] 1차 가드 counter monotonic + 증가 검증 통과 (final flow=${final_flow} pod=${final_pod})"
fi

# 2차 가드 (warn-only): 트래픽 발생 중 BPF map utilization 의 entries 누락 정합.
# starts LRU evict timing 의존 race 가 발생 하면 entries 가 0 으로 떨어지는 등 비정상 spike.
echo "[check] 2차 가드 (warn-only) BPF map utilization 정합"
starts_util=$(query_value 'netobs_bpf_map_utilization_ratio{map="starts"}')
if python3 -c "exit(0 if 0.0 <= float('${starts_util}') <= 1.0 else 1)"; then
  echo "[pass] starts map utilization=${starts_util} (정상 범위)"
else
  echo "[warn] starts map utilization=${starts_util} 비정상 범위"
fi

# 3차 가드 (warn-only): drop event divergence. ringbuf drop 외 비-설명 divergence 가 있는지.
echo "[check] 3차 가드 (warn-only) drop event divergence"
ringbuf_drops=$(query_value 'rate(netobs_bpf_ringbuf_drops_total[1m])')
echo "  ringbuf drop rate (1m): ${ringbuf_drops}"
if python3 -c "exit(0 if float('${ringbuf_drops}') < 100.0 else 1)"; then
  echo "[pass] ringbuf drop rate 정상 범위 (<100/s)"
else
  echo "[warn] ringbuf drop rate=${ringbuf_drops}/s 높음. 트래픽 강도 대비 reader throughput 검토 필요"
fi

echo "[pass] bpf-map-race 회귀 가드 1차 통과 (2-3차 warn-only)"
exit 0
