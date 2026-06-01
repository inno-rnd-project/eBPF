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

  # ConfigMap 의 dashboard JSON 을 inspect 해 annotation 정의 검증. python 측이 환경 변수
  # (EXPECTED_COUNT, EXPECTED_FILTER, SOURCE_FILE) 로 spec 을 받아 5 가드 (개수, firing 부착,
  # critical=red, warning=orange, component 필터) 전부를 수행 하고 결과를 단일 라인 (PASS 또는
  # FAIL <이유>) 으로만 반환 한다. bash 측은 PASS 여부 만 분기 해 awk 의 hardcoded 인덱스 의존을
  # 제거 하고 가드 항목 추가 시 python 만 변경 하도록 책임 단일화 한다. set -euo pipefail 환경
  # 에서 kubectl 실패 시 파이프라인 전체 종료 회피 위해 fallback echo 부착.
  result=$(kubectl get cm -n "${NAMESPACE}" "${cm}" -o json 2>/dev/null \
    | EXPECTED_COUNT="${expected_count}" EXPECTED_FILTER="${expected_filter}" SOURCE_FILE="${fname}" \
      python3 -c '
import json, os, sys
try:
    cm_doc = json.load(sys.stdin)
except Exception:
    print("FAIL invalid json or empty input")
    sys.exit(1)
data = cm_doc.get("data") or {}
fname = os.environ["SOURCE_FILE"]
content = data.get(fname) or ""
if not content:
    print(f"FAIL empty configmap or missing file ({fname})")
    sys.exit(1)
try:
    d = json.loads(content)
except Exception:
    print("FAIL invalid nested json")
    sys.exit(1)
annots = (d.get("annotations") or {}).get("list") or []
expected_count = int(os.environ.get("EXPECTED_COUNT", "2"))
expected_filter = os.environ.get("EXPECTED_FILTER", "")
count = len(annots)
if count != expected_count:
    print(f"FAIL annotation 개수={count} (기대 {expected_count})")
    sys.exit(1)
firing_count = sum(1 for a in annots if "alertstate=\"firing\"" in a.get("expr", ""))
if firing_count != expected_count:
    print(f"FAIL alertstate=\"firing\" 부착={firing_count} (기대 {expected_count})")
    sys.exit(1)
critical_red = sum(1 for a in annots if a.get("name") == "alerts-critical" and a.get("iconColor") == "red" and "severity=\"critical\"" in a.get("expr", ""))
if critical_red != 1:
    print(f"FAIL alerts-critical (red) 부착={critical_red} (기대 1)")
    sys.exit(1)
warning_orange = sum(1 for a in annots if a.get("name") == "alerts-warning" and a.get("iconColor") == "orange" and "severity=\"warning\"" in a.get("expr", ""))
if warning_orange != 1:
    print(f"FAIL alerts-warning (orange) 부착={warning_orange} (기대 1)")
    sys.exit(1)
if expected_filter:
    filter_match = sum(1 for a in annots if expected_filter in a.get("expr", ""))
    if filter_match != expected_count:
        print(f"FAIL component 필터 {expected_filter} 매칭={filter_match} (기대 {expected_count})")
        sys.exit(1)
print("PASS")
' 2>&1 || echo "FAIL configmap not found or kubectl error")

  if [[ "${result}" != "PASS" ]]; then
    echo "[fail] ${cm}: ${result}"
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
