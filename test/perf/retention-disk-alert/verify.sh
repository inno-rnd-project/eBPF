#!/usr/bin/env bash
# retention-disk-alert/verify.sh 는 이슈 #108 의 Prometheus retention 디스크와 OOM 위험 alert 도입
# 회귀 가드 다. 외부 dd 또는 fallocate 같은 PV 직접 spike 시뮬은 Prometheus TSDB 동작에 영향 가능
# 하므로 본 verify 는 임계값 임시 하향 패치 패턴으로 실제 발화를 검증 한다. (1) 1차 가드 alert rule
# 4종과 recording rule 1종이 Prometheus 의 `/api/v1/rules` 응답에 등록되어 있는지 fail-on-miss,
# (2) 2차 가드 (warn-only) 는 비결정적 timing 의존 이라 임계값 임시 하향 패치 후 발화 확인 패턴
# 절차만 docs 로 안내 하고 본 verify 에서는 직접 시뮬 하지 않 는다 (운영 안전성 우선).
# 본 스크립트는 dev cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

PROM_NAMESPACE="${PROM_NAMESPACE:-monitoring}"
PROM_SVC="${PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${PROM_PORT:-9090}"
TIMEOUT_SECONDS="${RULE_TIMEOUT:-180}"

PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"

if ! curl -sf --max-time 5 "${PROM_URL}/-/ready" >/dev/null 2>&1; then
  echo "[fatal] Prometheus 가 ${PROM_URL} 에서 도달 불가. ClusterIP 라 본 스크립트 는 클러스터 내부 에서 실행 필요"
  exit 1
fi
echo "[ok] prometheus /-/ready 응답 확인"

# 1차 가드 (fail-on-miss): alert rule 4종 과 recording rule 1종 의 등록 정합. prometheus-operator
# reconcile 후 rule이 Prometheus의 `/api/v1/rules` 응답에 도달 하기까지 통상 30-60s 소요.
EXPECTED_RULES=(
  "prometheus:host_disk_usage_ratio:5m"
  "PrometheusVolumeUsageHigh"
  "PrometheusVolumeUsageCritical"
  "PrometheusHighCardinality"
  "PrometheusMemoryPressure"
)

echo "[poll] 1차 가드 rule 5종 등록 확인 (timeout ${TIMEOUT_SECONDS}s)"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while (( $(date +%s) < deadline )); do
  resp=$(curl -sf --max-time 10 "${PROM_URL}/api/v1/rules" 2>/dev/null || echo "")
  registered=$(echo "${resp}" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    names = []
    for g in d.get('data', {}).get('groups', []):
        for r in g.get('rules', []):
            names.append(r.get('name',''))
    print('\n'.join(names))
except: pass" 2>/dev/null)

  missing=()
  for name in "${EXPECTED_RULES[@]}"; do
    if ! echo "${registered}" | grep -qFx "${name}"; then
      missing+=("${name}")
    fi
  done

  if (( ${#missing[@]} == 0 )); then
    echo "[pass] rule 5종 모두 등록 확인 (4 alert + 1 record)"
    break
  fi
  echo "[wait] missing: ${missing[*]}"
  sleep 15
done

if (( ${#missing[@]} != 0 )); then
  echo "[fail] rule ${TIMEOUT_SECONDS}s 안에 등록 미완료. missing: ${missing[*]}"
  exit 1
fi

# 1.5차 가드 (warn-only): recording rule 의 실제 산출 확인. Prometheus eval interval (30s) 이후 산출.
echo "[check] recording rule 산출 확인"
count=$(curl -sf -G "${PROM_URL}/api/v1/query" --data-urlencode 'query=prometheus:host_disk_usage_ratio:5m' 2>/dev/null \
  | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('data',{}).get('result',[])))" 2>/dev/null || echo 0)
if [[ "${count}" -ge 1 ]]; then
  val=$(curl -sf -G "${PROM_URL}/api/v1/query" --data-urlencode 'query=max(prometheus:host_disk_usage_ratio:5m)' 2>/dev/null \
    | python3 -c "import sys,json; r=json.load(sys.stdin).get('data',{}).get('result',[]); print(r[0]['value'][1] if r else '?')")
  echo "[pass] recording rule 산출 count=${count} max_ratio=${val}"
else
  echo "[warn] recording rule 미산출. node-exporter 의 node_filesystem_* 메트릭 부재 또는 eval interval 미경과"
fi

# 2차 가드 (warn-only / 안내): 실제 발화 확인은 임계값 임시 하향 패치 절차 로 docs 안내. 본 verify 는
# 직접 시뮬 하지 않 으나 운영자가 수동 검증 가능한 절차를 출력 한다.
echo "[info] 2차 가드 (수동) 실제 발화 검증 절차:"
echo "  1. kubectl edit prometheusrule -n monitoring prometheus-retention-capacity"
echo "     PrometheusVolumeUsageHigh.expr 의 0.8 을 0.05 같은 매우 낮은 값으로 임시 하향 (현재 사용률 보다 낮게)"
echo "  2. Prometheus 의 alert 평가 사이클 (15m for 조건) 안에서 발화 확인:"
echo "     curl -sG ${PROM_URL}/api/v1/alerts | python3 -c \"import sys,json; [print(a['labels']['alertname'],a['state']) for a in json.load(sys.stdin)['data']['alerts']]\""
echo "  3. 검증 후 즉시 0.8 로 원복하여 false positive 방지"

echo "[pass] retention-disk-alert 회귀 가드 1차 통과 (2차 warn-only 안내)"
exit 0
