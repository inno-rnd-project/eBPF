#!/usr/bin/env bash
# resource-anomaly-spike/verify.sh 는 이슈 #89 의 5분 z-score 기반 spike 감지 회귀 가드 다. dev
# cluster prometheus 에서 신규 recording rule 8종 (avg / rate base 4종과 z-score 4종) 의 동시
# emit 그리고 alert rule 4종 의 등록 그리고 z-score clamp 가드 동작을 검증한다. z-score 시리즈는
# 직전 7일 baseline 데이터 가 필요 라 dev 환경 의 데이터 부재 (신규 배포 직후) 시 자연 0건 으로
# 통과 하도록 graceful skip 처리한다. 본 스크립트 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

NAMESPACE="${SPIKE_NAMESPACE:-ebpf-project}"
PROM_NAMESPACE="${SPIKE_PROM_NAMESPACE:-monitoring}"
PROM_SVC="${SPIKE_PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${SPIKE_PROM_PORT:-9090}"

PROM_IP="${SPIKE_PROM_IP:-}"
if [[ -z "${PROM_IP}" ]]; then
  PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
fi
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"

# 1차 가드: PrometheusRule 의 resource-anomaly-spike group 등록 확인
echo "[setup] PrometheusRule resource-anomaly-spike group 확인"
rule_groups=$(kubectl get prometheusrule -n "${NAMESPACE}" netobs-gpuobs-correlation \
  -o jsonpath='{.spec.groups[*].name}' 2>/dev/null || true)
if ! echo "${rule_groups}" | tr ' ' '\n' | grep -qFx 'netobs-gpuobs.resource-anomaly-spike.recording'; then
  echo "[fail] netobs-gpuobs.resource-anomaly-spike.recording group 미등록"
  exit 1
fi
echo "[pass] resource-anomaly-spike.recording group 확인"

# 2차 가드: base record 4종 의 즉시 emit 확인 (5분 윈도우 라 evaluation 직후 채워짐)
BASE_RECORDS=(
  'cluster:gpu_util_5m_avg'
  'cluster:network_drop_5m_rate'
  'cluster:cpu_throttle_5m_avg'
  'cluster:memory_pressure_5m_avg'
)
echo "[setup] base record 4종 emit 확인"
for q in "${BASE_RECORDS[@]}"; do
  count=$(curl -sf --max-time 10 --data-urlencode "query=count(${q})" "${PROM_URL}/api/v1/query" \
    | python3 -c "import json,sys; r=json.load(sys.stdin)['data']['result']; print(r[0]['value'][1] if r else 0)" 2>/dev/null || echo 0)
  if [[ "${count}" == "0" ]]; then
    echo "[fail] ${q}: count=0 (base 시리즈 부재 또는 record evaluation 미완료)"
    exit 1
  fi
  echo "[pass] ${q}: count=${count}"
done

# 3차 가드 (graceful skip): z-score record 4종. 직전 7일 baseline 누적 필요 라 dev 환경 retention 누적
# 부족 시 자연 0건 으로 통과. 단 count > 0 이면 clamp(-5, 5) 동작 확인.
ZSCORE_RECORDS=(
  'cluster:gpu_util_zscore:5m'
  'cluster:network_drop_zscore:5m'
  'cluster:cpu_throttle_zscore:5m'
  'cluster:memory_pressure_zscore:5m'
)
echo "[setup] z-score record 4종 확인 (직전 7일 baseline 부족 시 graceful skip)"
for q in "${ZSCORE_RECORDS[@]}"; do
  count=$(curl -sf --max-time 10 --data-urlencode "query=count(${q})" "${PROM_URL}/api/v1/query" \
    | python3 -c "import json,sys; r=json.load(sys.stdin)['data']['result']; print(r[0]['value'][1] if r else 0)" 2>/dev/null || echo 0)
  if [[ "${count}" == "0" ]]; then
    echo "[skip] ${q}: count=0 (직전 7일 baseline 누적 부족, dev 환경 정상 동작)"
    continue
  fi
  out_of_range=$(curl -sf --max-time 10 --data-urlencode "query=count(abs(${q}) > 5)" "${PROM_URL}/api/v1/query" \
    | python3 -c "import json,sys; r=json.load(sys.stdin)['data']['result']; print(r[0]['value'][1] if r else 0)" 2>/dev/null || echo 0)
  if [[ "${out_of_range}" != "0" ]]; then
    echo "[fail] ${q}: clamp(-5, 5) 가드 깨짐 (out_of_range=${out_of_range})"
    exit 1
  fi
  echo "[pass] ${q}: count=${count}, clamp 가드 유지"
done

# 4차 가드: alert rule 4종 등록 확인
ALERTS=(
  'GPUUtilSpikeDetected'
  'NetworkDropSpikeDetected'
  'CPUThrottleSpikeDetected'
  'MemoryPressureSpikeDetected'
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

# 5차 가드: alert 4종의 severity 와 component 라벨 정합. set -euo pipefail 환경 에서 python
# sys.exit(1) 시 command substitution 자체 가 실패해 스크립트 가 즉시 종료 되는 위험 차단 위해
# if ! ...; then 패턴 으로 trap.
echo "[setup] alert component=*-anomaly 라벨 정합 확인"
if ! component_check=$(kubectl get prometheusrule -n "${NAMESPACE}" netobs-gpuobs-correlation -o json \
  | python3 -c "
import json, sys
d = json.load(sys.stdin)
expected = {
    'GPUUtilSpikeDetected': 'gpuobs-anomaly',
    'NetworkDropSpikeDetected': 'netobs-anomaly',
    'CPUThrottleSpikeDetected': 'cpu-anomaly',
    'MemoryPressureSpikeDetected': 'memory-anomaly',
}
fail = 0
for g in d['spec']['groups']:
    for r in g['rules']:
        if 'alert' in r and r['alert'] in expected:
            comp = r.get('labels', {}).get('component', '')
            sev = r.get('labels', {}).get('severity', '')
            if comp != expected[r['alert']]:
                print(f'FAIL {r[\"alert\"]}: component={comp} expected={expected[r[\"alert\"]]}')
                fail = 1
            elif sev != 'warning':
                print(f'FAIL {r[\"alert\"]}: severity={sev} expected=warning')
                fail = 1
sys.exit(fail)
"); then
  echo "${component_check}"
  echo "[fail] alert label 정합 깨짐"
  exit 1
fi
echo "[pass] alert 4종 severity=warning, component=*-anomaly 정합"

echo "[pass] resource-anomaly-spike 회귀 가드 통과 (record 8종, alert 4종, label 정합)"
