package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// ruleFileGlob 은 alert rule 을 정의하는 모든 컴포넌트의 base rule 파일을 매칭한다. 하드코딩
// 목록 대신 glob 을 써 새 컴포넌트의 rule 파일이 추가돼도 커버리지 검증이 자동 포함한다 (파일
// 누락으로 그 alert 가 조용히 검증을 빠지는 잠복 방지). 본 테스트 파일은 internal/correlation/api
// 라 3 단계 상위가 repo 루트다.
const ruleFileGlob = "../../../deploy/*/base/prometheus-rule*.yaml"

var alertLine = regexp.MustCompile(`(?m)^\s*- alert:\s*([A-Za-z0-9]+)\s*$`)

// ruleAlertnames 는 rule 파일들에서 정의된 alertname 집합을 뽑는다.
func ruleAlertnames(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(ruleFileGlob)
	if err != nil {
		t.Fatalf("rule 파일 glob 실패 %s: %v", ruleFileGlob, err)
	}
	if len(files) == 0 {
		t.Fatalf("rule 파일을 찾지 못함 (glob=%s, 경로 확인)", ruleFileGlob)
	}
	names := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("rule 파일 읽기 실패 %s: %v", f, err)
		}
		for _, m := range alertLine.FindAllStringSubmatch(string(data), -1) {
			names[m[1]] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("rule 파일에서 alertname 을 하나도 못 찾음 (경로/포맷 확인)")
	}
	return names
}

// TestIncidentCatalog_CoversAllRuleAlerts 는 #349 의 정합 검증이다. prometheus-rule.yaml 에 alert 를
// 추가하고 카탈로그 갱신을 잊으면 본 테스트가 CI 에서 누락을 잡는다. rule alertname 집합이
// incidentCatalog 키의 부분집합이어야 한다 (카탈로그가 rule 밖 내장 alert 를 더 가질 수는 있음).
func TestIncidentCatalog_CoversAllRuleAlerts(t *testing.T) {
	names := ruleAlertnames(t)
	missing := []string{}
	for name := range names {
		if _, ok := incidentCatalog[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("incidentCatalog 에 누락된 rule alert %d 종: %v\n(deploy/*/base/prometheus-rule.yaml 에 alert 추가 시 incidentcatalog.go 도 갱신)", len(missing), missing)
	}
}

// TestIncidentCatalog_NoStaleEntries 는 카탈로그에 rule 밖 항목이 쌓이지 않게 가드한다. rule 에서
// alert 를 제거하면 카탈로그 항목도 함께 지워 단일 소스를 유지한다. 다만 kube-prometheus-stack
// 내장 alert 는 rule 파일 밖이라 카탈로그 대상이 아니므로, 현재 카탈로그는 rule 집합과 정확히
// 일치해야 한다 (내장 alert 는 미등록 graceful 경로가 처리).
func TestIncidentCatalog_NoStaleEntries(t *testing.T) {
	names := ruleAlertnames(t)
	stale := []string{}
	for k := range incidentCatalog {
		if !names[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("incidentCatalog 에 rule 에 없는 stale 항목 %d 종: %v\n(rule 에서 제거된 alert 는 카탈로그에서도 제거)", len(stale), stale)
	}
}

// TestIncidentDescribe_RenderAndGraceful 은 summary 치환과 미등록 graceful 을 검증한다.
func TestIncidentDescribe_RenderAndGraceful(t *testing.T) {
	// noisy neighbor: suspect→victim 가해 관계를 담는다 (#349 리뷰). 라벨은 rule expr 의
	// max by 유지 셋 (suspect/victim namespace·pod, resource_dimension) 이다.
	nt, ns := incidentDescribe("CorrelationStrongNoisyNeighbor", map[string]string{
		"suspect_namespace": "batch", "suspect_pod": "trainer",
		"victim_namespace": "serving", "victim_pod": "api", "resource_dimension": "network",
	})
	if nt != "강한 noisy neighbor 간섭" || ns != "batch/trainer가 serving/api를 network 자원 경합으로 간섭" {
		t.Errorf("noisy neighbor title=%q summary=%q", nt, ns)
	}

	// 등록 alert: labels 로 summary 치환.
	// #416 부터 본 alert 는 페어링 맵 한정이라 fixture 도 starts 를 쓴다.
	title, summary := incidentDescribe("NetObsBpfMapUtilizationHigh", map[string]string{
		"map": "starts", "instance": "10.0.0.1:9810",
	})
	if title != "BPF 맵 사용률 임계 초과" {
		t.Errorf("title=%q", title)
	}
	if summary != "netobs BPF 페어링 맵 starts의 사용률이 10.0.0.1:9810에서 임계에 근접함 (미매칭 entry leak 신호)" {
		t.Errorf("summary=%q (치환 실패)", summary)
	}

	// 라벨 부재 시 unknown 폴백으로 문장 유지.
	_, summary = incidentDescribe("NetObsBpfMapUtilizationHigh", map[string]string{"map": "seg_accum"})
	if summary != "netobs BPF 페어링 맵 seg_accum의 사용률이 unknown에서 임계에 근접함 (미매칭 entry leak 신호)" {
		t.Errorf("summary=%q want unknown 폴백", summary)
	}

	// 미등록 alertname: title=alertname, summary 생략.
	title, summary = incidentDescribe("SomeBuiltinAlert", map[string]string{"x": "y"})
	if title != "SomeBuiltinAlert" || summary != "" {
		t.Errorf("graceful 실패: title=%q summary=%q", title, summary)
	}

	// 파이프 대안 {{node|instance}}: node 우선, node 부재 시 instance 폴백. ALERTS 시리즈의
	// 라벨셋이 annotation 참조와 어긋나는 케이스 (map 알림은 node, up 기반 알림은 instance) 를 모두
	// 견고하게 렌더한다.
	_, summary = incidentDescribe("ObsAgentDown", map[string]string{"job": "netobs-agent", "node": "worker1"})
	if summary != "netobs-agent가 worker1에서 다운됨" {
		t.Errorf("summary=%q want node 우선", summary)
	}
	_, summary = incidentDescribe("ObsAgentDown", map[string]string{"job": "netobs-agent", "instance": "10.0.0.1:9810"})
	if summary != "netobs-agent가 10.0.0.1:9810에서 다운됨" {
		t.Errorf("summary=%q want instance 폴백", summary)
	}
}
