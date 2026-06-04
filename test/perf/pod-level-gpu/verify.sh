#!/usr/bin/env bash
# pod-level-gpu/verify.sh 는 이슈 #104 의 Pod-level GPU utilization 도입 회귀 가드다. dev cluster 의
# RTX 3090 환경에서 (1) self-health 메트릭 (gpuobs_mig_mode, gpuobs_mps_active) 의 정상 발행, (2)
# active CUDA workload 적용 후 gpuobs_pod_utilization_percent 시리즈 emit, (3) pod:gpu_util_p95:5m
# recording rule 산출 까지 3 단계 가드를 수행한다. MIG 활성 환경의 instance level 시리즈는 dev cluster
# 환경 미보유로 본 가드 범위 밖이며 graceful degradation 경로 (mig_mode=unsupported) 만 cover 한다.
# 본 스크립트는 dev cluster 전용이며 prod 에서 실행하지 않는다.
set -euo pipefail

NAMESPACE="${GPUOBS_NAMESPACE:-ebpf-project}"
PROM_NAMESPACE="${PROM_NAMESPACE:-monitoring}"
PROM_SVC="${PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${PROM_PORT:-9090}"
TIMEOUT_SECONDS="${GPUOBS_TIMEOUT:-300}"
POLL_INTERVAL="${GPUOBS_POLL_INTERVAL:-15}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
BENCH_YAML="${REPO_ROOT}/test/perf/pytorch-conv2d-bench.yaml"

PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"

# query_count 는 Prometheus 의 instant query 결과 시리즈 개수 를 반환 한다.
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

# 1차 가드: MIG mode / MPS active self-health 메트릭 발행 확인.
# set -u 환경 에서 polling 루프 가 한 번도 실행 되지 않은 (TIMEOUT_SECONDS=0 등) 경우 의 unbound var
# 실패 방어 위해 mig_count / mps_count 를 루프 진입 전 0 으로 명시 초기화 한다.
echo "[poll] 1차 가드 self-health 메트릭 (gpuobs_mig_mode + gpuobs_mps_active) 발행 확인"
mig_count=0
mps_count=0
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while (( $(date +%s) < deadline )); do
  mig_count=$(query_count 'gpuobs_mig_mode')
  mps_count=$(query_count 'gpuobs_mps_active')
  if [[ "${mig_count}" -ge 1 && "${mps_count}" -ge 1 ]]; then
    echo "[pass] self-health emit: mig_mode=${mig_count} mps_active=${mps_count}"
    break
  fi
  echo "[wait] mig_mode=${mig_count} mps_active=${mps_count} (둘 다 ≥1 필요)"
  sleep "${POLL_INTERVAL}"
done
if [[ "${mig_count}" -lt 1 || "${mps_count}" -lt 1 ]]; then
  echo "[fail] self-health 메트릭 ${TIMEOUT_SECONDS}s 안에 미발행"
  exit 1
fi

# dev cluster (RTX 3090) graceful degradation 확인: mig_mode=unsupported 시리즈 가 1 이상 active.
unsupported_count=$(query_count 'gpuobs_mig_mode{mode="unsupported"} == 1')
if [[ "${unsupported_count}" -lt 1 ]]; then
  echo "[warn] mig_mode=unsupported 시리즈 부재. dev cluster 가 RTX 3090 환경 이 아니거나 MIG enabled GPU 합류 가능성"
else
  echo "[pass] graceful degradation 경로 활성: mig_mode=unsupported count=${unsupported_count}"
fi

# 2차 가드: active CUDA workload 적용 후 gpuobs_pod_utilization_percent 시리즈 emit 확인.
echo "[setup] CUDA bench workload (pytorch-conv2d-bench) 적용"
kubectl apply -f "${BENCH_YAML}" >/dev/null
trap "echo '[cleanup] bench workload 정리'; kubectl delete -f \"${BENCH_YAML}\" --ignore-not-found >/dev/null 2>&1 || true" EXIT

echo "[poll] 2차 가드 gpuobs_pod_utilization_percent 시리즈 emit 대기 (timeout ${TIMEOUT_SECONDS}s)"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
util_count=0
while (( $(date +%s) < deadline )); do
  util_count=$(query_count 'gpuobs_pod_utilization_percent{src_pod=~"pytorch-.*"}')
  if [[ "${util_count}" -ge 1 ]]; then
    echo "[pass] gpuobs_pod_utilization_percent emit: count=${util_count}"
    break
  fi
  echo "[wait] util emit 미관측. bench Pod 가 cuda init 중일 수 있음"
  sleep "${POLL_INTERVAL}"
done
if [[ "${util_count}" -lt 1 ]]; then
  echo "[fail] gpuobs_pod_utilization_percent 시리즈 ${TIMEOUT_SECONDS}s 안에 미발행"
  exit 1
fi

# 3차 가드 (warn-only): pod:gpu_util_p95:5m recording rule 산출 확인. 본 rule 은 5분 윈도우 라 bench
# workload 가 5분 이상 가동 되어야 산출 되므로 dev cluster 짧은 검증 사이클 에서는 warn 처리.
echo "[poll] 3차 가드 (warn-only) pod:gpu_util_p95:5m recording rule 산출 확인"
p95_count=$(query_count 'pod:gpu_util_p95:5m')
if [[ "${p95_count}" -ge 1 ]]; then
  echo "[pass] pod:gpu_util_p95:5m count=${p95_count}"
else
  echo "[warn] pod:gpu_util_p95:5m 시리즈 부재. 5분 윈도우 미충족 (bench 가동 시간 부족) 가능성"
fi

echo "[pass] pod-level-gpu 회귀 가드 1-2 단계 통과 (3 단계 warn 처리)"
exit 0
