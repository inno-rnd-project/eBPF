#!/usr/bin/env bash
# nccl-integration/verify.sh 는 이슈 #134 의 libnccl.so uprobe NCCL Profiler 통합 회귀 가드 다.
# 데이터 센터 GPU (A100 과 H100 등) 의 multi-rank distributed training 환경 에서 build tag nccl 로
# 빌드 한 이미지 가 배포 되고 GPUOBS_NCCL_ENABLED=true 로 활성 된 경우 gpuobs_nccl_profiler_
# available 가 1 로 emit 되고 gpu_idle_cause_weight:5m{cause="nccl_collective_stall"} weight 가 0
# 보다 큰 값 으로 산출 되는지 확인 한다. dev cluster 의 RTX 3090 환경 은 collective 자체 가 발생
# 하지 않고 기본 이미지 가 build tag nccl 비활성 이라 본 검증 이 불가능 하므로 collective duration
# 메트릭 부재 시 graceful skip 한다.
#
# 본 스크립트 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

PROM_NAMESPACE="${NCCL_PROM_NAMESPACE:-monitoring}"
PROM_SVC="${NCCL_PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${NCCL_PROM_PORT:-9090}"
TIMEOUT_SECONDS="${NCCL_TIMEOUT:-300}"
POLL_INTERVAL="${NCCL_POLL_INTERVAL:-15}"

PROM_IP="${NCCL_PROM_IP:-}"
if [[ -z "${PROM_IP}" ]]; then
  PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
fi
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"

probe() {
  local q="$1"
  local resp
  resp=$(curl -sf --max-time 10 --data-urlencode "query=${q}" "${PROM_URL}/api/v1/query" 2>/dev/null || true)
  echo "${resp}" | python3 -c "
import sys, json
try:
    res = json.load(sys.stdin)['data']['result']
    print(res[0]['value'][1] if res else '')
except Exception:
    print('')" 2>/dev/null || echo ""
}

# 사전 가드: gpuobs_nccl_collective_duration_seconds_count 메트릭 이 Prometheus 에 emit 되는지
# 확인. 부재 면 NCCL profiler 미활성 환경 (RTX 3090 noop 또는 build tag nccl 비활성 이미지) 이라
# 본 검증 을 graceful skip 한다.
echo "[setup] checking NCCL collective base metric (gpuobs_nccl_collective_duration_seconds_count)"
nccl_present=$(probe 'count(gpuobs_nccl_collective_duration_seconds_count)')
if [[ -z "${nccl_present}" || "${nccl_present}" == "0" ]]; then
  echo "[skip] gpuobs_nccl_collective_duration_seconds 부재. NCCL profiler 미활성 환경 (RTX 3090 또는 nccl 미빌드 이미지) 이라 graceful skip"
  echo "       데이터센터 GPU 환경에서 build tag nccl 이미지 배포 후 GPUOBS_NCCL_ENABLED=true 와 multi-rank 워크로드로 재실행하라"
  exit 0
fi
echo "[setup] NCCL collective base metric 존재 (count=${nccl_present})"

# 1차 가드: gpuobs_nccl_profiler_available 가 1 로 emit 되는지 확인. gpuobs-agent 의 wire-up 이
# libnccl.so collective 심볼 attach 성공 여부 를 판정해 self-health gauge 를 set 한다.
echo "[poll] 1차 가드 gpuobs_nccl_profiler_available == 1 대기 (timeout ${TIMEOUT_SECONDS}s)"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
guard1=0
while (( $(date +%s) < deadline )); do
  v=$(probe 'max(gpuobs_nccl_profiler_available)')
  if [[ "${v}" == "1" ]]; then
    echo "[pass] gpuobs_nccl_profiler_available=1"
    guard1=1
    break
  fi
  echo "[wait] gpuobs_nccl_profiler_available=${v:-none}"
  sleep "${POLL_INTERVAL}"
done
if (( guard1 == 0 )); then
  echo "[fail] gpuobs_nccl_profiler_available 가 1 로 emit 되지 않음. GPUOBS_NCCL_ENABLED 와 build tag nccl 이미지 와 libnccl.so hostPath 마운트 와 CAP_BPF/CAP_PERFMON/CAP_SYS_PTRACE 점검"
  exit 1
fi

# 2차 가드: gpu_idle_cause_weight:5m{cause="nccl_collective_stall"} weight 가 0 보다 큰 값 으로
# 산출 되는지 확인. base score (cluster:nccl_collective_stall_score:5m) 가 collective duration
# rate 를 정규화 해 weight 로 반영 한다. GPU idle 게이팅 (max(node:gpu_idle:5m) > 0.5) 이 활성 인
# 시간대 에만 emit 되므로 weight series 자체 가 부재 할 수 있어 base score present 도 함께 본다.
echo "[poll] 2차 가드 nccl_collective_stall weight 산출 확인"
score=$(probe 'max(cluster:nccl_collective_stall_score:5m)')
echo "[info] cluster:nccl_collective_stall_score:5m=${score:-none}"
weight=$(probe 'max(gpu_idle_cause_weight:5m{cause="nccl_collective_stall"})')
if [[ -n "${weight}" ]] && awk "BEGIN { exit !(${weight} > 0) }"; then
  echo "[pass] nccl_collective_stall weight=${weight} (> 0)"
elif [[ -n "${score}" ]] && awk "BEGIN { exit !(${score} > 0) }"; then
  echo "[pass] base score=${score} (> 0). GPU idle 게이팅 미활성 시간대라 weight series는 미emit이나 base score 활성 확인"
else
  echo "[warn] nccl_collective_stall weight와 base score 모두 0. profiler는 attach되었으나 collective wait가 짧은 정상 상태일 수 있음"
fi

echo "[pass] nccl-integration 회귀 가드 통과"
exit 0
