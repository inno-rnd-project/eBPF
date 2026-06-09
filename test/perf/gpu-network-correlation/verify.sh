#!/usr/bin/env bash
# gpu-network-correlation/verify.sh 는 이슈 #86 의 GPU x network cross-correlation 통합 패널 회귀
# 가드 다. dev cluster 의 prometheus 에서 신규 recording rule 4 종 (node:gpu_util_ratio:5m,
# node:gpu_memory_used_ratio:5m, pod:network_throughput_bps:5m, pod:network_p99_latency_seconds:5m)
# 이 동시에 non-empty 시리즈로 emit 되는지 확인한다. correlation overlay 의 noisy_neighbor_score
# 는 dev cluster idle 시 비어 있을 수 있어 본 가드 의 hard 조건에서 제외하고 present 확인 만
# warn 으로 처리한다. recording rule 적용 직후 5 분 warmup 이 필요하므로 기본 timeout 을 600s 로
# 둔다. 본 스크립트 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다. 본 가드의 의도는
# recording rule 4 종의 산정 회귀 차단이라 특정 namespace 한정 가시성은 검증 대상이 아니며
# dev cluster 의 자연 트래픽이 어느 namespace 든 emit 되면 통과 한다.
set -euo pipefail

NAMESPACE="${GPUNET_NAMESPACE:-ebpf-project}"
TIMEOUT_SECONDS="${GPUNET_TIMEOUT:-600}"
POLL_INTERVAL="${GPUNET_POLL_INTERVAL:-30}"

PROM_NAMESPACE="${GPUNET_PROM_NAMESPACE:-monitoring}"
PROM_SVC="${GPUNET_PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${GPUNET_PROM_PORT:-9090}"

PROM_IP="${GPUNET_PROM_IP:-}"
if [[ -z "${PROM_IP}" ]]; then
  # set -euo pipefail 환경에서 kubectl 실패 (service 부재 / kubeconfig 오류 등) 시 command
  # substitution 실패로 스크립트 가 즉시 종료 되어 아래 [fatal] 메시지 가 누락 된다. fallback
  # 으로 빈 문자열 을 두어 다음 if 가드 가 actionable 한 에러 메시지 를 노출 하도록 한다.
  PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
fi
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"
echo "[setup] timeout=${TIMEOUT_SECONDS}s"

# PrometheusRule netobs-gpuobs-correlation 의 신규 group 이 실제로 적용 되어 있는지 사전 확인 한다.
# rule 미적용 상태 에서 5 분 warmup 만 기다리면 hard fail 까지 시간 손실이 크다.
echo "[setup] checking netobs-gpuobs-correlation PrometheusRule"
rule_groups=$(kubectl get prometheusrule -n "${NAMESPACE}" netobs-gpuobs-correlation \
  -o jsonpath='{.spec.groups[*].name}' 2>/dev/null || true)
if [[ -z "${rule_groups}" ]]; then
  echo "[fatal] PrometheusRule netobs-gpuobs-correlation 가 ${NAMESPACE} 에 존재 하지 않는다"
  exit 1
fi
if ! echo "${rule_groups}" | tr ' ' '\n' | grep -qFx 'netobs-gpuobs.cross-correlation.recording'; then
  echo "[fatal] netobs-gpuobs.cross-correlation.recording group 이 누락 되어 있다 (groups=${rule_groups})"
  exit 1
fi
echo "[setup] netobs-gpuobs.cross-correlation.recording group 확인"

echo "[poll] waiting up to ${TIMEOUT_SECONDS}s for cross-correlation series"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))

# 1차 가드 4종: 신규 recording rule 시리즈 가 각각 1 개 이상 emit 되는지 확인. dev cluster 의
# netobs allow_namespaces 설정에 따라 특정 namespace 가 emit 대상이 아닐 수 있어 namespace
# 필터는 두지 않고 "어떤 namespace 든 4 시계열이 동시에 non-empty 면 통과" 로 둔다. 2차 가드
# (warn only): correlation overlay 가 present 한지 확인 하되 비어 있어도 hard fail 시키지 않는다
# (idle cluster 정상 동작 케이스).
query_gpu_util="count(node:gpu_util_ratio:5m)"
query_gpu_mem="count(node:gpu_memory_used_ratio:5m)"
query_net_throughput="count(pod:network_throughput_bps:5m)"
query_net_p99="count(pod:network_p99_latency_seconds:5m)"
query_overlay="count(correlation_noisy_neighbor_score{resource_dimension=~\"gpu|network\"})"

extract_value() {
  echo "$1" | python3 -c "import json,sys
try:
    res = json.load(sys.stdin)['data']['result']
    if res:
        print(res[0]['value'][1])
except Exception:
    pass" 2>/dev/null || echo ""
}

probe() {
  local q="$1"
  local resp
  resp=$(curl -sf --max-time 10 --data-urlencode "query=${q}" "${PROM_URL}/api/v1/query" 2>/dev/null || true)
  extract_value "${resp}"
}

guard1_passed=0
while (( $(date +%s) < deadline )); do
  gpu_util_count=$(probe "${query_gpu_util}")
  gpu_mem_count=$(probe "${query_gpu_mem}")
  net_throughput_count=$(probe "${query_net_throughput}")
  net_p99_count=$(probe "${query_net_p99}")
  overlay_count=$(probe "${query_overlay}")

  if [[ -n "${gpu_util_count}" && -n "${gpu_mem_count}" \
        && -n "${net_throughput_count}" && -n "${net_p99_count}" ]]; then
    if awk "BEGIN { exit !(${gpu_util_count} >= 1 && ${gpu_mem_count} >= 1 \
            && ${net_throughput_count} >= 1 && ${net_p99_count} >= 1) }"; then
      echo "[pass] gpu_util=${gpu_util_count} gpu_mem=${gpu_mem_count} \
net_throughput=${net_throughput_count} net_p99=${net_p99_count}"
      if [[ -n "${overlay_count}" ]] && awk "BEGIN { exit !(${overlay_count} >= 1) }"; then
        echo "[pass] correlation overlay present (count=${overlay_count})"
      else
        echo "[warn] correlation overlay 가 비어 있다 (idle cluster 일 가능성). overlay_count=${overlay_count:-0}"
      fi
      guard1_passed=1
      break
    fi
    echo "[wait] gpu_util=${gpu_util_count} gpu_mem=${gpu_mem_count} \
net_throughput=${net_throughput_count} net_p99=${net_p99_count} overlay=${overlay_count:-0}"
  else
    echo "[wait] recording rule 시리즈 가 아직 emit 되지 않음 (5 분 warmup 대기)"
  fi
  sleep "${POLL_INTERVAL}"
done

if (( guard1_passed == 0 )); then
  echo "[fail] timed out waiting for cross-correlation recording rule series."
  echo "       PrometheusRule 적용 후 5 분 warmup 경과 와 dev cluster 의 자연 트래픽 활성 여부 를 점검 하라."
  exit 1
fi

# 2차 가드 (#120): Pod join sanity check. 신규 pod:gpu_network_correlation_score:5m 의 query 가 정상
# 응답 (식 syntactic 정합) 하는지 확인 한 뒤 pod:gpu_util_p95:5m 가 1 이상 시리즈 면 동일 4 Pod 라벨
# join key 가 GPU 메트릭 과 network 메트릭 양쪽 에서 모두 발견 되는지 fail-on-miss 검증 한다. active
# CUDA workload 부재 환경 (pod:gpu_util_p95:5m 0 series) 에서는 graceful skip 처리 한다. count() 의
# empty vector 결과 는 시리즈 0 개를 의미 하므로 빈 문자열 을 0 으로 정규화 한다.
echo "[guard2] Pod join sanity check (#120)"
# query 자체 의 HTTP 응답 정합 검증. score query 응답 status 가 success 면 syntactic 정합 통과.
score_status=$(curl -sf --max-time 10 --data-urlencode 'query=pod:gpu_network_correlation_score:5m' "${PROM_URL}/api/v1/query" 2>/dev/null | python3 -c "import json,sys
try:
    print(json.load(sys.stdin).get('status',''))
except Exception:
    print('')")
if [[ "${score_status}" != "success" ]]; then
  echo "[fail] pod:gpu_network_correlation_score:5m query 가 success 응답 미반환 (status=${score_status})"
  exit 1
fi
score_count=$(probe "count(pod:gpu_network_correlation_score:5m)")
score_count="${score_count:-0}"
echo "[guard2] pod:gpu_network_correlation_score:5m count=${score_count}"

gpu_pod_count=$(probe "count(pod:gpu_util_p95:5m)")
gpu_pod_count="${gpu_pod_count:-0}"
if awk "BEGIN { exit !(${gpu_pod_count} >= 1) }"; then
  join_count=$(probe "count(pod:gpu_util_p95:5m and on(node, src_namespace, src_pod, src_pod_uid) pod:network_throughput_bps:5m)")
  join_count="${join_count:-0}"
  if awk "BEGIN { exit !(${join_count} >= 1) }"; then
    echo "[pass] Pod join sanity check 통과 (gpu_pod=${gpu_pod_count} join=${join_count} score=${score_count})"
  else
    echo "[fail] GPU Pod-level 메트릭 ${gpu_pod_count} 시리즈 가 있으나 동일 4 Pod 라벨 join key 가 network 메트릭 과 매칭 되지 않는다"
    exit 1
  fi
else
  echo "[skip] pod:gpu_util_p95:5m 시리즈 부재 (active CUDA workload 부재 환경). graceful skip 처리"
fi
exit 0
