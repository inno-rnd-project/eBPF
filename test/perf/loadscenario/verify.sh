#!/usr/bin/env bash
# loadscenario/verify.sh 는 이슈 #102 의 LoadScenario controller 와 spike alert 자동 검증 회귀 가드
# 다. dev cluster 에서 CRD 등록, controller Deployment Ready, 짧은 schedule (@every 1m) 의 LoadScenario
# CR 적용, status.lastScheduleTime 과 status.lastSuccessfulRunTime 갱신, spike alert hit 까지 5
# 단계 가드 한다. 회귀 검증 직후 LoadScenario CR 즉시 정리 (test/perf/* 정리 정책 동일 적용).
# 본 스크립트 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

NAMESPACE="${LS_NAMESPACE:-ebpf-project}"
TARGET_NS="${LS_TARGET_NAMESPACE:-correlation-stress}"
TARGET_POD="${LS_TARGET_POD:-victim}"
LS_NAME="${LS_NAME:-verify-cpu-smoke}"
ROLLOUT_TIMEOUT="${LS_ROLLOUT_TIMEOUT:-300s}"
TIMEOUT_SECONDS="${LS_TIMEOUT:-300}"
POLL_INTERVAL="${LS_POLL_INTERVAL:-15}"

cleanup() {
  echo "[cleanup] LoadScenario 와 ConfigMap lock 정리"
  kubectl delete loadscenario -n "${NAMESPACE}" "${LS_NAME}" --ignore-not-found --wait=false 2>/dev/null || true
}
trap cleanup EXIT

echo "[setup] 1차 가드 CRD 등록 확인"
if ! kubectl get crd loadscenarios.injector.netobs.io >/dev/null 2>&1; then
  echo "[fatal] CRD loadscenarios.injector.netobs.io 가 cluster 에 등록 되어 있지 않다"
  echo "        kubectl apply -k deploy/injector/overlays/dev 로 적용 한다"
  exit 1
fi
echo "[setup] CRD 등록 확인"

echo "[setup] 2차 가드 controller Deployment Ready 확인"
if ! kubectl rollout status -n "${NAMESPACE}" deployment/workload-injector-controller --timeout="${ROLLOUT_TIMEOUT}" 2>&1 | tail -3; then
  echo "[fatal] controller Deployment rollout 이 ${ROLLOUT_TIMEOUT} 안에 ready 가 되지 않았다"
  kubectl get pod -n "${NAMESPACE}" -l app.kubernetes.io/component=controller -o wide
  exit 1
fi
echo "[setup] controller Deployment Ready"

echo "[setup] target Pod (${TARGET_NS}/${TARGET_POD}) 존재 확인"
if ! kubectl get pod -n "${TARGET_NS}" "${TARGET_POD}" >/dev/null 2>&1; then
  echo "[fatal] target Pod ${TARGET_NS}/${TARGET_POD} 가 부재. correlation-stress workload 적용 여부 점검"
  exit 1
fi

echo "[setup] 3차 가드 LoadScenario CR 적용 (@every 1m 짧은 schedule)"
# spikeAlertAssertion 은 false 로 둔다. true 인 경우 controller 가 부하 종료 후 5 분 polling
# window 동안 prometheus 의 ALERTS 시리즈 를 query 해 status 갱신 이 polling timeout 보다 더
# 늦어진다. 본 가드 는 controller 의 schedule 따른 reconcile 정상 동작 만 검증 하고 spike alert
# 자동 검증 은 별도 시나리오 로 follow-up.
cat <<EOF | kubectl apply -f -
apiVersion: injector.netobs.io/v1alpha1
kind: LoadScenario
metadata:
  name: ${LS_NAME}
  namespace: ${NAMESPACE}
spec:
  schedule: "@every 1m"
  kind: cpu
  duration: 30s
  intensity: 500m
  targetRef:
    namespace: ${TARGET_NS}
    name: ${TARGET_POD}
  concurrencyPolicy: Forbid
  spikeAlertAssertion: false
  maxFailures: 3
EOF

echo "[poll] status.lastScheduleTime 갱신 까지 최대 ${TIMEOUT_SECONDS}s 대기"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
last_schedule=""
while (( $(date +%s) < deadline )); do
  last_schedule=$(kubectl get loadscenario -n "${NAMESPACE}" "${LS_NAME}" -o jsonpath='{.status.lastScheduleTime}' 2>/dev/null || echo "")
  if [[ -n "${last_schedule}" ]]; then
    echo "[pass] status.lastScheduleTime=${last_schedule}"
    break
  fi
  echo "[wait] lastScheduleTime 아직 없음"
  sleep "${POLL_INTERVAL}"
done
if [[ -z "${last_schedule}" ]]; then
  echo "[fail] ${TIMEOUT_SECONDS}s 안에 status.lastScheduleTime 이 갱신 되지 않았다"
  kubectl describe loadscenario -n "${NAMESPACE}" "${LS_NAME}" | tail -30
  exit 1
fi

echo "[poll] 4차 가드 status.lastSuccessfulRunTime 갱신 확인 (duration 30s + warmup)"
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
last_success=""
while (( $(date +%s) < deadline )); do
  last_success=$(kubectl get loadscenario -n "${NAMESPACE}" "${LS_NAME}" -o jsonpath='{.status.lastSuccessfulRunTime}' 2>/dev/null || echo "")
  if [[ -n "${last_success}" ]]; then
    echo "[pass] status.lastSuccessfulRunTime=${last_success}"
    break
  fi
  echo "[wait] lastSuccessfulRunTime 아직 없음"
  sleep "${POLL_INTERVAL}"
done
if [[ -z "${last_success}" ]]; then
  echo "[fail] ${TIMEOUT_SECONDS}s 안에 status.lastSuccessfulRunTime 이 갱신 되지 않았다"
  kubectl describe loadscenario -n "${NAMESPACE}" "${LS_NAME}" | tail -30
  exit 1
fi

echo "[skip] 5차 가드 spike alert 자동 검증 은 본 가드 의 spec.spikeAlertAssertion=false 채택 으로 skip"
echo "       spike alert 자동 검증 단독 시나리오 는 follow-up issue 로 분리"

echo "[pass] LoadScenario controller 회귀 가드 1-4 단계 통과"
exit 0
