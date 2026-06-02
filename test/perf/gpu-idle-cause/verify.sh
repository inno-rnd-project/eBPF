#!/usr/bin/env bash
# gpu-idle-cause/verify.sh 는 이슈 #101 의 victim 단위 GPU idle cause weight 보강 회귀 가드 다.
# dev cluster 의 prometheus 에서 신규 recording rule 7 종 (pod:gpu_idle_cause_score:5m,
# pod:gpu_idle_cause_sum:5m, pod:gpu_idle_cause_weight:5m, victim:gpu_idle_dominant_cause:5m,
# victim:gpu_idle_dominant_cause_indicator:5m, gpu_idle_cause_weight_top3:5m,
# pod:gpu_idle_cause_weight_top3:5m) 이 동시에 non-empty 시리즈로 emit 되는지 확인 한다.
# alert rule 2 종 (GPUIdleDominantCauseAmbiguous, VictimGPUIdleDominantCauseAmbiguous) 의
# 등록 여부 도 함께 확인 한다. dashboard ConfigMap 의 panel 2 종 등록 여부 도 직접 inspect
# 한다. recording rule 적용 직후 5 분 warmup 이 필요 하므로 기본 timeout 을 600s 로 둔다.
# 본 스크립트 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다. GPU 부하 워크로드를 본 가드가
# 추가 spawn 하지 않으며 dev cluster 의 자연 cause 신호 또는 기존 correlation-stress workload 의
# 신호 만으로 통과 한다.
set -euo pipefail

NAMESPACE="${GPUIDLE_NAMESPACE:-ebpf-project}"
TIMEOUT_SECONDS="${GPUIDLE_TIMEOUT:-600}"
POLL_INTERVAL="${GPUIDLE_POLL_INTERVAL:-30}"

PROM_NAMESPACE="${GPUIDLE_PROM_NAMESPACE:-monitoring}"
PROM_SVC="${GPUIDLE_PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${GPUIDLE_PROM_PORT:-9090}"

PROM_IP="${GPUIDLE_PROM_IP:-}"
if [[ -z "${PROM_IP}" ]]; then
  PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
fi
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"
echo "[setup] timeout=${TIMEOUT_SECONDS}s"

# PrometheusRule netobs-gpuobs-correlation 의 신규 rule 이 적용 되어 있는지 사전 확인. rule 미적용
# 상태 에서 5 분 warmup 대기 만 하면 hard fail 까지 시간 손실이 크다.
echo "[setup] PrometheusRule netobs-gpuobs-correlation rule 등록 확인"
rule_dump=$(kubectl get prometheusrule -n "${NAMESPACE}" netobs-gpuobs-correlation -o yaml 2>/dev/null || true)
if [[ -z "${rule_dump}" ]]; then
  echo "[fatal] PrometheusRule netobs-gpuobs-correlation 가 ${NAMESPACE} 에 존재 하지 않는다"
  exit 1
fi

required_records=(
  "pod:gpu_idle_cause_score:5m"
  "pod:gpu_idle_cause_sum:5m"
  "pod:gpu_idle_cause_weight:5m"
  "victim:gpu_idle_dominant_cause:5m"
  "victim:gpu_idle_dominant_cause_indicator:5m"
  "gpu_idle_cause_weight_top3:5m"
  "pod:gpu_idle_cause_weight_top3:5m"
)
required_alerts=(
  "GPUIdleDominantCauseAmbiguous"
  "VictimGPUIdleDominantCauseAmbiguous"
)
for rec in "${required_records[@]}"; do
  # bash substring 검사. echo | grep -qF 는 set -e + pipefail 환경 에서 grep 의 early exit
  # 가 echo 의 SIGPIPE 를 일으켜 pipeline 이 비정상 종료 반환 하므로 회피.
  if [[ "${rule_dump}" != *"record: ${rec}"* ]]; then
    echo "[fatal] recording rule '${rec}' 가 PrometheusRule 에 누락"
    exit 1
  fi
done
for al in "${required_alerts[@]}"; do
  if [[ "${rule_dump}" != *"alert: ${al}"* ]]; then
    echo "[fatal] alert rule '${al}' 가 PrometheusRule 에 누락"
    exit 1
  fi
done
echo "[setup] recording rule 7 종 과 alert rule 2 종 등록 확인"

# dashboard ConfigMap inspect. #87 verify.sh 패턴 차용. panel id 2 와 3 의 신규 panel 이 ConfigMap
# 안에 등록 되어 있는지 확인 한다.
echo "[setup] gpu-network-correlation dashboard ConfigMap panel 등록 확인"
dashboard_json=$(kubectl get configmap -n "${NAMESPACE}" gpu-network-correlation-dashboard \
  -o jsonpath='{.data.gpu-network-correlation\.json}' 2>/dev/null || true)
if [[ -z "${dashboard_json}" ]]; then
  echo "[fatal] gpu-network-correlation-dashboard ConfigMap 이 ${NAMESPACE} 에 존재 하지 않는다"
  exit 1
fi
panel_count=$(echo "${dashboard_json}" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['panels']))" 2>/dev/null || echo "0")
if [[ "${panel_count}" -lt 3 ]]; then
  echo "[fatal] dashboard panel 수가 ${panel_count} 로 3 미만 (panel id 2 와 3 누락 의심)"
  exit 1
fi
required_panel_exprs=(
  "pod:gpu_idle_cause_weight:5m"
  "victim:gpu_idle_dominant_cause_indicator:5m"
)
for expr in "${required_panel_exprs[@]}"; do
  if [[ "${dashboard_json}" != *"${expr}"* ]]; then
    echo "[fatal] dashboard panel 표현식 '${expr}' 누락"
    exit 1
  fi
done
echo "[setup] dashboard panel ${panel_count} 개 등록 과 신규 표현식 2 종 포함 확인"

# 시리즈 emit 가드. dev cluster 의 어느 victim 이라도 신규 시리즈 가 1 개 이상 emit 되면 통과.
# idle 게이팅 (max(node:gpu_idle:5m) > 0.5) 이 실패 한 시간대 는 weight 와 dominant cause 시리즈
# 가 0 으로 떨어 진다. 본 가드 는 idle 게이팅 활성 시간대 의 emit 만 검증 한다.
echo "[poll] waiting up to ${TIMEOUT_SECONDS}s for victim 단위 cause weight series"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))

query_pod_score="count(pod:gpu_idle_cause_score:5m)"
query_pod_sum="count(pod:gpu_idle_cause_sum:5m)"
query_pod_weight="count(pod:gpu_idle_cause_weight:5m)"
query_victim_dominant="count(victim:gpu_idle_dominant_cause:5m)"
query_victim_indicator="count(victim:gpu_idle_dominant_cause_indicator:5m)"
query_cluster_top3="count(gpu_idle_cause_weight_top3:5m)"
query_pod_top3="count(pod:gpu_idle_cause_weight_top3:5m)"
query_ambig_alert="count(ALERTS{alertname=~\"GPUIdleDominantCauseAmbiguous|VictimGPUIdleDominantCauseAmbiguous\",alertstate=~\"pending|firing\"})"

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

while (( $(date +%s) < deadline )); do
  pod_score=$(probe "${query_pod_score}")
  pod_sum=$(probe "${query_pod_sum}")
  pod_weight=$(probe "${query_pod_weight}")
  victim_dom=$(probe "${query_victim_dominant}")
  victim_ind=$(probe "${query_victim_indicator}")
  cluster_top3=$(probe "${query_cluster_top3}")
  pod_top3=$(probe "${query_pod_top3}")
  ambig_alert=$(probe "${query_ambig_alert}")

  if [[ -n "${pod_score}" && -n "${pod_sum}" && -n "${pod_weight}" \
        && -n "${victim_dom}" && -n "${victim_ind}" \
        && -n "${cluster_top3}" && -n "${pod_top3}" ]]; then
    if awk "BEGIN { exit !(${pod_score} >= 1 && ${pod_sum} >= 1 && ${pod_weight} >= 1 \
            && ${victim_dom} >= 1 && ${victim_ind} >= 1 \
            && ${cluster_top3} >= 1 && ${pod_top3} >= 1) }"; then
      echo "[pass] pod_score=${pod_score} pod_sum=${pod_sum} pod_weight=${pod_weight} \
victim_dom=${victim_dom} victim_ind=${victim_ind} cluster_top3=${cluster_top3} pod_top3=${pod_top3}"
      if [[ -n "${ambig_alert}" ]] && awk "BEGIN { exit !(${ambig_alert} >= 1) }"; then
        echo "[pass] ambiguous alert pending 또는 firing 확인 (count=${ambig_alert})"
      else
        echo "[warn] ambiguous alert 가 발화 되지 않음 (dev cluster 의 cause 가 모두 동률 미만 또는 magnitude 미달). ambig_alert=${ambig_alert:-0}"
      fi
      exit 0
    fi
    echo "[wait] pod_score=${pod_score} pod_sum=${pod_sum} pod_weight=${pod_weight} \
victim_dom=${victim_dom} victim_ind=${victim_ind} cluster_top3=${cluster_top3} pod_top3=${pod_top3}"
  else
    echo "[wait] recording rule 시리즈 가 아직 emit 되지 않음 (5 분 warmup 또는 idle 게이팅 비활성)"
  fi
  sleep "${POLL_INTERVAL}"
done

echo "[fail] timed out waiting for victim 단위 cause weight recording rule series."
echo "       PrometheusRule 적용 후 5 분 warmup 경과, gpuobs-agent 의 node:gpu_idle:5m emit, "
echo "       그리고 dev cluster 의 자연 cause 신호 활성 여부 를 점검 하라."
exit 1
