#!/usr/bin/env bash
# alert-annotation/verify.sh 는 이슈 #90 의 dashboard alert annotation 표시 회귀 가드 다. Grafana
# API auth 의존성을 회피 하기 위해 7 dashboard ConfigMap 을 직접 inspect 해 annotation 정의의
# 정합성을 검증한다. 각 dashboard 별 annotation 개수 (모두 2: critical + warning) 와 expr 의
# alertstate="firing" 부착 그리고 severity 별 iconColor 매핑 (critical=red, warning=orange) 그리고
# component 라벨 필터 매핑 표 정합 을 동시 가드 한다. 본 스크립트 는 dev cluster 전용 이며 prod
# 에서 실행 하지 않는다.
set -euo pipefail

NAMESPACE="${ANNOT_NAMESPACE:-ebpf-project}"

# dashboard ConfigMap 마다 expected annotation 개수와 component 필터 fragment 매핑. component 필터
# 가 빈 문자열 ("") 이면 전체 (observability-overview, rca-dashboard 의 RCA 진입점 특례) 의미.
declare -A EXPECTED_ANNOTATION_COUNT=(
  [overview-dashboard]=2
  [netobs-dashboard]=2
  [gpuobs-dashboard]=2
  [correlation-dashboard]=2
  [gpu-network-correlation-dashboard]=2
  [injector-dashboard]=2
  [rca-dashboard]=2
)

declare -A EXPECTED_COMPONENT_FILTER=(
  [overview-dashboard]=""
  [netobs-dashboard]='component=~"netobs|netobs-capacity|observability"'
  [gpuobs-dashboard]='component=~"gpuobs|gpuobs-capacity"'
  [correlation-dashboard]='component="correlation"'
  [gpu-network-correlation-dashboard]='component=~"netobs|gpuobs|netobs-capacity|gpuobs-capacity"'
  [injector-dashboard]='component="injector"'
  [rca-dashboard]=""
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

echo "[setup] 7 dashboard ConfigMap inspect 를 통한 alert annotation 회귀 가드"
echo "[setup] namespace=${NAMESPACE}"

fail=0

for cm in "${!EXPECTED_ANNOTATION_COUNT[@]}"; do
  expected_count="${EXPECTED_ANNOTATION_COUNT[$cm]}"
  expected_filter="${EXPECTED_COMPONENT_FILTER[$cm]}"
  fname="${SOURCE_FILE[$cm]}"

  # ConfigMap 의 dashboard JSON 을 inspect 해 annotation 정의 검증. python 으로 dict access 후
  # severity / iconColor / alertstate / component 필터 패턴 일치 여부 가드. set -euo pipefail
  # 환경 에서 kubectl 실패 시 파이프라인 전체 종료 회피 위해 fallback echo 부착.
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
annots = (d.get('annotations') or {}).get('list') or []
count = len(annots)
# alertstate=\"firing\" 부착 가드
firing_count = sum(1 for a in annots if 'alertstate=\"firing\"' in a.get('expr', ''))
# severity 별 iconColor 매핑 가드
critical_red = sum(1 for a in annots if a.get('name') == 'alerts-critical' and a.get('iconColor') == 'red' and 'severity=\"critical\"' in a.get('expr', ''))
warning_orange = sum(1 for a in annots if a.get('name') == 'alerts-warning' and a.get('iconColor') == 'orange' and 'severity=\"warning\"' in a.get('expr', ''))
# component 필터 가드
expected_filter = '''${expected_filter}'''
if expected_filter:
    filter_match = sum(1 for a in annots if expected_filter in a.get('expr', ''))
else:
    filter_match = 'skip'
print(f'count={count} firing={firing_count} critical_red={critical_red} warning_orange={warning_orange} filter_match={filter_match}')
" 2>&1 || echo "FAIL configmap not found or kubectl error")

  if [[ "${result}" == FAIL* ]]; then
    echo "[fail] ${cm}: ${result}"
    fail=1
    continue
  fi

  count=$(echo "${result}" | awk -F= '/^count/ {print $2}' | awk '{print $1}')
  firing=$(echo "${result}" | awk -F= '/firing/ {print $3}' | awk '{print $1}')
  critical_red=$(echo "${result}" | awk -F= '/critical_red/ {print $4}' | awk '{print $1}')
  warning_orange=$(echo "${result}" | awk -F= '/warning_orange/ {print $5}' | awk '{print $1}')
  filter_match=$(echo "${result}" | awk -F= '/filter_match/ {print $NF}')

  if [[ "${count}" != "${expected_count}" ]]; then
    echo "[fail] ${cm}: annotation 개수=${count} (기대 ${expected_count})"
    fail=1
    continue
  fi
  if [[ "${firing}" != "${expected_count}" ]]; then
    echo "[fail] ${cm}: alertstate=\"firing\" 부착=${firing} (기대 ${expected_count})"
    fail=1
    continue
  fi
  if [[ "${critical_red}" != "1" ]]; then
    echo "[fail] ${cm}: alerts-critical (red) 부착=${critical_red} (기대 1)"
    fail=1
    continue
  fi
  if [[ "${warning_orange}" != "1" ]]; then
    echo "[fail] ${cm}: alerts-warning (orange) 부착=${warning_orange} (기대 1)"
    fail=1
    continue
  fi
  if [[ -n "${expected_filter}" && "${filter_match}" != "2" ]]; then
    echo "[fail] ${cm}: component 필터 ${expected_filter} 매칭=${filter_match} (기대 2)"
    fail=1
    continue
  fi
  echo "[pass] ${cm}: annotation 2 rule, firing/severity/iconColor/component 정합"
done

if (( fail )); then
  echo "[fail] alert annotation 회귀 가드 실패"
  exit 1
fi
echo "[pass] 7 dashboard 의 alert annotation 정합성 회귀 가드 통과"
