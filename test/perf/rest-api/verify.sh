#!/usr/bin/env bash
# rest-api/verify.sh 는 이슈 #100 의 REST API layer 회귀 가드 다. 3 agent (correlation-exporter 와
# netobs-agent 와 gpuobs-agent) 의 신규 endpoint 4종 (noisy-neighbor / flows / drops / gpu) 과
# swagger UI 와 통합 swagger-ui Pod 가 dev cluster 에서 정상 emit 되는지 확인한다. 본 스크립트
# 는 dev cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

NAMESPACE="${API_NAMESPACE:-ebpf-project}"

echo "[setup] 3 agent 의 REST endpoint 4종 과 swagger UI 회귀 가드"
echo "[setup] namespace=${NAMESPACE}"

fail=0

# 4 endpoint 각각에 GET 요청 후 200 응답 과 JSON Content-Type 확인. cluster 내부 통신용 ClusterIP
# 를 자동 발견 한다.
probe_endpoint() {
  local svc="$1"
  local port="$2"
  local path="$3"
  local ip
  ip=$(kubectl get svc -n "${NAMESPACE}" "${svc}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
  if [[ -z "${ip}" ]]; then
    echo "[fail] ${svc}: ClusterIP 발견 불가"
    return 1
  fi
  local url="http://${ip}:${port}${path}"
  local http_code
  http_code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 "${url}" 2>/dev/null || echo "000")
  if [[ "${http_code}" != "200" ]]; then
    echo "[fail] ${svc} ${path}: HTTP ${http_code}"
    return 1
  fi
  local content_type
  content_type=$(curl -sI --max-time 10 "${url}" 2>/dev/null | grep -i 'content-type' | tr -d '\r' || echo "")
  if ! echo "${content_type}" | grep -qi 'application/json'; then
    echo "[fail] ${svc} ${path}: Content-Type 이 application/json 아님 (${content_type})"
    return 1
  fi
  echo "[pass] ${svc} ${path}: HTTP 200, JSON"
  return 0
}

echo "[setup] endpoint 응답 가드"
probe_endpoint correlation-exporter 9830 /api/v1/noisy-neighbor || fail=1
probe_endpoint netobs-agent 9810 /api/v1/flows || fail=1
probe_endpoint netobs-agent 9810 /api/v1/drops || fail=1
probe_endpoint gpuobs-agent 9820 /api/v1/gpu || fail=1

# swagger.json 가드 (3 agent: correlation-exporter 와 netobs-agent 와 gpuobs-agent)
echo "[setup] swagger.json 응답 가드"
probe_endpoint correlation-exporter 9830 /api/v1/swagger.json || fail=1
probe_endpoint netobs-agent 9810 /api/v1/swagger.json || fail=1
probe_endpoint gpuobs-agent 9820 /api/v1/swagger.json || fail=1

# 통합 swagger-ui Service 가드
echo "[setup] 통합 swagger-ui Service 응답 가드"
swagger_ip=$(kubectl get svc -n "${NAMESPACE}" swagger-ui -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
if [[ -z "${swagger_ip}" ]]; then
  echo "[skip] swagger-ui Service 미배포 (kubectl apply -k deploy/swagger-ui/ 적용 후 재실행)"
else
  http_code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 "http://${swagger_ip}:8080/" 2>/dev/null || echo "000")
  if [[ "${http_code}" != "200" ]]; then
    echo "[fail] swagger-ui Service /: HTTP ${http_code}"
    fail=1
  else
    echo "[pass] swagger-ui Service /: HTTP 200"
  fi
fi

if (( fail )); then
  echo "[fail] REST API layer 회귀 가드 실패"
  exit 1
fi
echo "[pass] REST API layer 회귀 가드 통과"
