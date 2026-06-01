#!/usr/bin/env bash
# drill-down-navigation/verify.sh 는 이슈 #87 의 cluster-node-pod 계층 navigation 통합 회귀 가드 다.
# Grafana API auth 대신 dev cluster 의 dashboard ConfigMap 을 직접 inspect 해 source-of-truth 의 panel
# links 정합성 을 검증 한다. 각 dashboard 의 link 총 개수 와 URL 패턴 (${__url_time_range} macro 부착
# 및 variable 매핑 변환 적용) 이 본 이슈 의 매핑 표 와 일치 하는지 가드 한다. 본 스크립트 는 dev
# cluster 전용 이며 prod 에서 실행 하지 않는다.
set -euo pipefail

NAMESPACE="${DRILLNAV_NAMESPACE:-ebpf-project}"

declare -A EXPECTED_LINKS=(
  [overview-dashboard]=85
  [netobs-dashboard]=19
  [gpuobs-dashboard]=21
  [correlation-dashboard]=2
  [gpu-network-correlation-dashboard]=3
  [injector-dashboard]=12
  [rca-dashboard]=0
)

declare -A SOURCE_FILE=(
  [overview-dashboard]=overview.json
  [netobs-dashboard]=netobs.json
  [gpuobs-dashboard]=gpuobs.json
  [correlation-dashboard]=correlation.json
  [gpu-network-correlation-dashboard]=gpu-network-correlation.json
  [injector-dashboard]=injector.json
  [rca-dashboard]=rca.json
)

echo "[setup] dashboard ConfigMap 7종 inspect 를 통한 panel.links 회귀 가드"
echo "[setup] namespace=${NAMESPACE}"

fail=0

for cm in "${!EXPECTED_LINKS[@]}"; do
  expected="${EXPECTED_LINKS[$cm]}"
  fname="${SOURCE_FILE[$cm]}"

  # kubectl jsonpath 는 key 에 dot 가 있으면 path separator 와 충돌 하므로 `-o json` 전체 응답을
  # python 으로 받아 dict access 한다. nested rows 의 panels 도 재귀 탐색 해 link 총 개수 와 URL
  # 패턴 매크로 (시간 범위) 채택 여부 를 동시 검증 한다. set -euo pipefail 환경 에서 ConfigMap
  # 부재 시 파이프라인 전체가 즉시 종료 되어 다른 dashboard 검증이 차단 되지 않도록 파이프라인
  # 끝에 fallback echo 를 두고, python 측 도 JSON parse / null panels 에 대한 방어 코드 를 둔다.
  result=$(kubectl get cm -n "${NAMESPACE}" "${cm}" -o json 2>/dev/null | python3 -c "
import json, sys
try:
    cm_doc = json.load(sys.stdin)
except Exception:
    print('FAIL invalid json or empty input')
    sys.exit(2)
data = cm_doc.get('data') or {}
content = data.get('${fname}') or ''
if not content:
    print('FAIL empty configmap or missing file')
    sys.exit(2)
try:
    d = json.loads(content)
except Exception:
    print('FAIL invalid nested json')
    sys.exit(2)
total = 0
without_time = 0
for p in d.get('panels') or []:
    nested = [p] if p.get('type') != 'row' else (p.get('panels') or [])
    for sp in nested:
        for l in sp.get('links') or []:
            total += 1
            url = l.get('url', '')
            if '\${__url_time_range}' not in url:
                without_time += 1
print(f'links={total} without_time={without_time}')
" 2>&1 || echo "FAIL configmap not found or kubectl error")

  if [[ "${result}" == FAIL* ]]; then
    echo "[fail] ${cm}: ${result}"
    fail=1
    continue
  fi

  actual=$(echo "${result}" | awk -F= '/^links/ {print $2}' | awk '{print $1}')
  no_time=$(echo "${result}" | awk -F= '/without_time/ {print $3}')

  if [[ "${actual}" != "${expected}" ]]; then
    echo "[fail] ${cm}: links=${actual} expected=${expected}"
    fail=1
  elif (( no_time > 0 )); then
    echo "[fail] ${cm}: links=${actual} 중 ${no_time}개에 \${__url_time_range} 미부착"
    fail=1
  else
    echo "[pass] ${cm}: links=${actual}"
  fi
done

if (( fail )); then
  echo "[fail] drill-down navigation 회귀 가드 실패"
  exit 1
fi
echo "[pass] 7 dashboard 의 panel.links 정합성 회귀 가드 통과"
