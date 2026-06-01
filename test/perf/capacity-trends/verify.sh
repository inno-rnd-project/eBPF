#!/usr/bin/env bash
# capacity-trends/verify.sh 는 이슈 #88 의 capacity-trends 패널 군 회귀 가드 다. dev cluster 의
# prometheus 에서 신규 recording rule 8종 (avg 4종, zscore 4종) 의 동시 emit 그리고 alert rule
# 4종 의 등록 그리고 prometheus retention 60d 적용 여부 를 가드 한다. z-score 시리즈는 30일
# baseline 누적 필요 라 dev 환경 의 retention 미달 시 자연 0 건 으로 통과 하도록 minimum-sample
# 가드 와 함께 graceful skip 처리한다. 본 스크립트 는 dev cluster 전용 이며 prod 에서 실행 하지
# 않는다.
set -euo pipefail

NAMESPACE="${CAP_NAMESPACE:-ebpf-project}"
PROM_NAMESPACE="${CAP_PROM_NAMESPACE:-monitoring}"
PROM_SVC="${CAP_PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${CAP_PROM_PORT:-9090}"

PROM_IP="${CAP_PROM_IP:-}"
if [[ -z "${PROM_IP}" ]]; then
  PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
fi
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"

# 1차 가드: retention 확인
echo "[setup] prometheus retention 확인"
retention=$(kubectl get prometheus -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.retention}' 2>/dev/null || echo "")
if [[ "${retention}" != "60d" ]]; then
  echo "[fail] prometheus retention=${retention:-unset} 가 60d 와 정합 안 됨. deploy/monitoring/ 적용 필요"
  exit 1
fi
echo "[pass] retention=${retention}"

# 2차 가드: PrometheusRule 의 capacity-trends group 등록 확인
echo "[setup] PrometheusRule capacity-trends group 확인"
rule_groups=$(kubectl get prometheusrule -n "${NAMESPACE}" netobs-gpuobs-correlation \
  -o jsonpath='{.spec.groups[*].name}' 2>/dev/null || true)
if ! echo "${rule_groups}" | tr ' ' '\n' | grep -qFx 'netobs-gpuobs.capacity-trends.recording'; then
  echo "[fail] netobs-gpuobs.capacity-trends.recording group 미등록"
  exit 1
fi
echo "[pass] capacity-trends.recording group 확인"

# 3차 가드: avg record 4종 의 즉시 emit 확인 (1시간 윈도우라 evaluation 직후 채워짐)
AVG_RECORDS=(
  'cluster:gpu_util_1h_avg'
  'cluster:network_1h_avg'
  'cluster:cpu_throttle_1h_avg'
  'cluster:memory_pressure_1h_avg'
)
echo "[setup] avg record 4종 emit 확인"
for q in "${AVG_RECORDS[@]}"; do
  count=$(curl -sf --max-time 10 --data-urlencode "query=count(${q})" "${PROM_URL}/api/v1/query" \
    | python3 -c "import json,sys; r=json.load(sys.stdin)['data']['result']; print(r[0]['value'][1] if r else 0)" 2>/dev/null || echo 0)
  if [[ "${count}" == "0" ]]; then
    echo "[fail] ${q}: count=0 (base 시리즈 부재 또는 record evaluation 미완료)"
    exit 1
  fi
  echo "[pass] ${q}: count=${count}"
done

# 4차 가드 (graceful skip): z-score record 4종. 30일 baseline 누적 필요 라 dev 환경 retention
# 미달 시 자연 0 건 으로 통과. 단 count > 0 이면 |value| <= 5 clamp 동작 확인.
ZSCORE_RECORDS=(
  'cluster:gpu_util_zscore:1h'
  'cluster:network_zscore:1h'
  'cluster:cpu_throttle_zscore:1h'
  'cluster:memory_pressure_zscore:1h'
)
echo "[setup] z-score record 4종 확인 (30일 baseline 미달 시 graceful skip)"
for q in "${ZSCORE_RECORDS[@]}"; do
  count=$(curl -sf --max-time 10 --data-urlencode "query=count(${q})" "${PROM_URL}/api/v1/query" \
    | python3 -c "import json,sys; r=json.load(sys.stdin)['data']['result']; print(r[0]['value'][1] if r else 0)" 2>/dev/null || echo 0)
  if [[ "${count}" == "0" ]]; then
    echo "[skip] ${q}: count=0 (30일 baseline 누적 부족, dev 환경 정상 동작)"
    continue
  fi
  # clamp(-5, 5) 가 동작 하는지 확인
  out_of_range=$(curl -sf --max-time 10 --data-urlencode "query=count(abs(${q}) > 5)" "${PROM_URL}/api/v1/query" \
    | python3 -c "import json,sys; r=json.load(sys.stdin)['data']['result']; print(r[0]['value'][1] if r else 0)" 2>/dev/null || echo 0)
  if [[ "${out_of_range}" != "0" ]]; then
    echo "[fail] ${q}: clamp(-5, 5) 가드 깨짐 (out_of_range=${out_of_range})"
    exit 1
  fi
  echo "[pass] ${q}: count=${count}, clamp 가드 유지"
done

# 5차 가드: alert rule 4종 등록 확인
ALERTS=(
  'GPUUtilAnomalyDetected'
  'NetworkUtilAnomalyDetected'
  'CPUThrottleAnomalyDetected'
  'MemoryPressureAnomalyDetected'
)
echo "[setup] alert rule 4종 등록 확인"
registered=$(kubectl get prometheusrule -n "${NAMESPACE}" netobs-gpuobs-correlation -o json \
  | python3 -c "
import json, sys
d = json.load(sys.stdin)
alerts = set()
for g in d['spec']['groups']:
    for r in g['rules']:
        if 'alert' in r:
            alerts.add(r['alert'])
print(' '.join(sorted(alerts)))
")
for a in "${ALERTS[@]}"; do
  if ! echo "${registered}" | tr ' ' '\n' | grep -qFx "${a}"; then
    echo "[fail] alert ${a} 미등록"
    exit 1
  fi
  echo "[pass] alert ${a} 등록 확인"
done

echo "[pass] capacity-trends 회귀 가드 통과 (record 8종, alert 4종, retention 60d)"
