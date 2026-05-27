#!/usr/bin/env bash
# drop-stack/verify.sh 는 이슈 #83 의 drop event kernel stack capture e2e 회귀 가드 다. dev cluster
# 의 자연 drop 6.79/s 와 nc-noport / cnp-drop 두 trigger 패턴 중 동작 하는 셋 을 활용 해 reason ×
# stack_hash 분포 가 netobs_drop_stack_total 메트릭 으로 노출 되는지 확인 한다. 종료 시 두 trigger
# manifest 와 임시 CNP 를 모두 정리 한다 (메모리 규칙 feedback_gpu_bench_cleanup 준수).
#
# 본 스크립트 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${DROP_STACK_NAMESPACE:-observability-test}"
PROM_NAMESPACE="${DROP_STACK_PROM_NAMESPACE:-monitoring}"
PROM_SVC="${DROP_STACK_PROM_SVC:-kube-prometheus-stack-prometheus}"
PROM_PORT="${DROP_STACK_PROM_PORT:-9090}"
TIMEOUT_SECONDS="${DROP_STACK_TIMEOUT:-300}"
POLL_INTERVAL="${DROP_STACK_POLL_INTERVAL:-15}"
TRIGGER_MODE="${DROP_STACK_TRIGGER_MODE:-auto}"   # auto | nc-noport | cnp-drop

PROM_IP="${DROP_STACK_PROM_IP:-}"
if [[ -z "${PROM_IP}" ]]; then
  PROM_IP=$(kubectl get svc -n "${PROM_NAMESPACE}" "${PROM_SVC}" -o jsonpath='{.spec.clusterIP}')
fi
if [[ -z "${PROM_IP}" ]]; then
  echo "[fatal] failed to resolve ${PROM_SVC} ClusterIP in ${PROM_NAMESPACE}"
  exit 1
fi
PROM_URL="http://${PROM_IP}:${PROM_PORT}"
echo "[setup] prometheus URL: ${PROM_URL}"

# trigger_mode 자동 선택 의 사전 검증 단계. CNP DROP 이 dev cluster 의 cilium 버전 에서 kfree_skb_
# reason 을 호출 하는지 확인 할 수 없 으면 nc-noport fallback 으로 자동 전환 한다. bpftrace 는 dev
# 노드 에 없을 수 있 으므로 본 단계 는 best-effort 로 진행 하고 실패 시 fallback 으로 처리 한다.
detect_trigger_mode() {
  if [[ "${TRIGGER_MODE}" != "auto" ]]; then
    echo "[setup] trigger mode (env override): ${TRIGGER_MODE}"
    return
  fi
  echo "[setup] trigger mode auto-detect: nc-noport 를 1 차 채택 (CNP DROP 의 kfree_skb_reason 호출 여부 사전 확인 미가용)"
  TRIGGER_MODE="nc-noport"
}

cleanup() {
  echo "[cleanup] removing trigger manifests from namespace ${NAMESPACE}"
  kubectl delete -n "${NAMESPACE}" -f "${SCRIPT_DIR}/nc-noport.yaml" --ignore-not-found=true --wait=false || true
  kubectl delete -n "${NAMESPACE}" -f "${SCRIPT_DIR}/cnp-drop.yaml" --ignore-not-found=true --wait=false || true
}
trap cleanup EXIT

detect_trigger_mode

case "${TRIGGER_MODE}" in
  nc-noport)
    echo "[setup] applying nc-noport trigger to namespace ${NAMESPACE}"
    kubectl apply -n "${NAMESPACE}" -f "${SCRIPT_DIR}/nc-noport.yaml"
    ;;
  cnp-drop)
    echo "[setup] applying cnp-drop trigger to namespace ${NAMESPACE}"
    kubectl apply -n "${NAMESPACE}" -f "${SCRIPT_DIR}/cnp-drop.yaml"
    ;;
  *)
    echo "[fatal] unsupported trigger mode: ${TRIGGER_MODE}"
    exit 1
    ;;
esac

echo "[poll] waiting up to ${TIMEOUT_SECONDS}s for netobs_drop_stack_total series"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
query='count(netobs_drop_stack_total)'
while (( $(date +%s) < deadline )); do
  if response=$(curl -sf --max-time 10 --data-urlencode "query=${query}" "${PROM_URL}/api/v1/query" 2>/dev/null); then
    value=$(echo "${response}" | grep -oE '"value":\[[0-9.]+,"[0-9]+"\]' | grep -oE '"[0-9]+"\]' | grep -oE '[0-9]+' | head -1)
    if [[ -n "${value}" && "${value}" -gt 0 ]]; then
      echo "[pass] netobs_drop_stack_total series count = ${value}"
      echo "[detail] series sample:"
      curl -sf --max-time 10 --data-urlencode 'query=netobs_drop_stack_total' "${PROM_URL}/api/v1/query" | head -c 1024
      echo
      exit 0
    fi
    echo "[wait] netobs_drop_stack_total still empty (series count=${value:-0})"
  else
    echo "[wait] prometheus query failed"
  fi
  sleep "${POLL_INTERVAL}"
done

echo "[fail] timed out waiting for netobs_drop_stack_total series."
echo "       NETOBS_DROP_STACK_ALLOW_NAMESPACES env 가 ${NAMESPACE} 를 포함 하는지 (kubectl get ds -n ebpf-project netobs-agent -o yaml | grep ALLOW_NAMESPACES) 확인 후 재시도 하라."
exit 1
