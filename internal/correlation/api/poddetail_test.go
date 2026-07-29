package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func podDetailQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("kube_pod_info", sample(1,
			"namespace", "ml", "pod", "train-a", "uid", "u-1", "node", "gpu",
			"pod_ip", "172.16.3.9", "created_by_kind", "ReplicaSet", "created_by_name", "train-a-abc")).
		on("pod:cpu_throttle_score:5m", sample(0.3, "src_namespace", "ml", "src_pod", "train-a")).
		on("pod:memory_pressure_score:5m", sample(0.6, "src_namespace", "ml", "src_pod", "train-a")).
		// network score 미산출 (트래픽 없는 pod 재현) → health 엔트리 생략. on 은 등록 순서 우선의
		// contains 매칭이라, 나눗셈 합성 쿼리는 고유한 나눗셈 조각을 단독 쿼리보다 먼저 등록한다.
		on("container_cpu_usage_seconds_total", sample(42.5)).
		on(`) / sum(kube_pod_container_resource_limits{namespace="ml", pod="train-a", resource="memory"})`, sample(75)).
		on("sum(container_memory_working_set_bytes", sample(4e9)).
		on("sum(increase(container_cpu_cfs_throttled_periods_total", sample(36)).
		on("sum(increase(container_cpu_cfs_periods_total", sample(300)).
		on(`resource="cpu"})`, sample(2)).
		on(`stage="retrans"`, sample(0.5)).
		on(`stage="drop"`, sample(0.1)).
		on("netobs_tcp_state_max_srtt_seconds", sample(0.25))
}

// TestPodDetail 은 기본 정보와 health 환산 (1 - score, 미산출 차원 생략), vitals 와 cpu 상세와
// network 합성을 검증한다. quota/period 는 시리즈 부재 (이 클러스터 재현) 로 생략된다.
func TestPodDetail(t *testing.T) {
	h := NewSynthesisHandler(podDetailQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetPodDetail(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pod/ml/train-a", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp PodDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.UID != "u-1" || resp.Node != "gpu" || resp.CreatedByName != "train-a-abc" {
		t.Errorf("기본 정보=%+v want kube_pod_info 라벨", resp)
	}
	if v, ok := resp.Health["cpu"]; !ok || v < 0.699 || v > 0.701 {
		t.Errorf("health cpu=%v want 0.7 (1-0.3)", resp.Health)
	}
	if v, ok := resp.Health["memory"]; !ok || v < 0.399 || v > 0.401 {
		t.Errorf("health memory=%v want 0.4", resp.Health)
	}
	if _, ok := resp.Health["network"]; ok {
		t.Errorf("health network=%v want 생략 (score 미산출)", resp.Health)
	}
	// ratio 는 36/300 의 파생 계산이다.
	if resp.Cpu.ThrottledRatio == nil || *resp.Cpu.ThrottledRatio != 0.12 {
		t.Errorf("throttled_ratio=%v want 0.12 (36/300)", resp.Cpu.ThrottledRatio)
	}
	if resp.Cpu.ThrottledPeriods5m == nil || *resp.Cpu.ThrottledPeriods5m != 36 || resp.Cpu.TotalPeriods5m == nil || *resp.Cpu.TotalPeriods5m != 300 {
		t.Errorf("periods=%+v want 36/300", resp.Cpu)
	}
	if resp.Cpu.LimitCores == nil || *resp.Cpu.LimitCores != 2 {
		t.Errorf("limit_cores=%v want 2", resp.Cpu.LimitCores)
	}
	if resp.Cpu.QuotaMicroseconds != nil || resp.Cpu.PeriodMicroseconds != nil {
		t.Errorf("quota/period=%+v want 생략 (시리즈 부재)", resp.Cpu)
	}
	if resp.Network.RetransPerSec == nil || *resp.Network.RetransPerSec != 0.5 || resp.Network.DropPerSec == nil || *resp.Network.DropPerSec != 0.1 {
		t.Errorf("network=%+v want 재전송 0.5/drop 0.1", resp.Network)
	}
	if resp.Network.MaxSrttSeconds == nil || *resp.Network.MaxSrttSeconds != 0.25 {
		t.Errorf("max_srtt=%v want 0.25", resp.Network.MaxSrttSeconds)
	}
	// #328 CPU 절대 사용량. 픽스처의 contains 매칭상 percent 와 같은 rule (42.5) 을 공유한다.
	if resp.Vitals.CPUUsageCores == nil || *resp.Vitals.CPUUsageCores != 42.5 {
		t.Errorf("cpu_usage_cores=%v want 42.5", resp.Vitals.CPUUsageCores)
	}
	// cadvisor 계열 sum 은 pod-level 행 가드 (container="") 를 붙여 표준 cadvisor 구성의 두 계층
	// 중복 합산을 막고, spec 계열 (quota) 은 pod-level 행이 없는 구성 대비로 가드 없이 조회한다.
	q := podDetailQuerier()
	h = NewSynthesisHandler(q, nil, nil)
	h.GetPodDetail(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/pod/ml/train-a", nil))
	if !q.sawQuery(`container_memory_working_set_bytes{namespace="ml", pod="train-a", container=""}`) {
		t.Error(`cadvisor sum 쿼리에 container="" 가드 부재 (계층 중복 합산 위험)`)
	}
	if !q.sawQuery(`container_spec_cpu_quota{namespace="ml", pod="train-a"}`) {
		t.Error("spec 계열 (max) 은 가드 없이 조회되어야 함 (pod-level 행 부재 구성 대비)")
	}
	if !strings.Contains(resp.Summary, "ml/train-a") || !strings.Contains(resp.Summary, "throttle 12%") {
		t.Errorf("summary=%q want 종합 요약", resp.Summary)
	}
}

// TestPodDetail_UnknownPod 는 미관측 pod 가 필드 생략과 summary 사유로 graceful 처리되는지 검증한다.
func TestPodDetail_UnknownPod(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPodDetail(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pod/ns/none", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (graceful)", rec.Code)
	}
	var resp PodDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Node != "" || len(resp.Health) != 0 {
		t.Errorf("resp=%+v want 빈 합성", resp)
	}
	if !strings.Contains(resp.Summary, "관측 데이터가 없습니다") {
		t.Errorf("summary=%q want 사유", resp.Summary)
	}
}

// TestPodDetail_InvalidPath 는 세그먼트 수 위반과 DNS-1123 위반이 쿼리 실행 전에 400 으로
// 거부되는지 검증한다.
func TestPodDetail_InvalidPath(t *testing.T) {
	q := &fakeQuerier{}
	h := NewSynthesisHandler(q, nil, nil)
	for _, target := range []string{"/api/v1/pod/only-one", "/api/v1/pod/a/b/c", "/api/v1/pod/ns/UPPER_case"} {
		rec := httptest.NewRecorder()
		h.GetPodDetail(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d want 400", target, rec.Code)
		}
	}
	if len(q.queries) != 0 {
		t.Errorf("거부 후 쿼리 실행됨: %v", q.queries)
	}
}

// TestPodDetail_NilQuerier 는 querier 미주입 시 graceful 빈 응답을 검증한다.
func TestPodDetail_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPodDetail(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pod/ns/p", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
}

// TestPodDetail_NoLimitCpuCores 는 #328 의 limit 없는 pod (CNI 와 kube-proxy 등) 케이스를 검증한다.
// limit 분모의 cpu_percent 와 memory_percent 는 생략되고 CPU 절대 사용량 (cores) 은 limit 유무와
// 무관하게 산출된다. 나눗셈 합성 쿼리 규약대로 percent 의 고유 나눗셈 조각을 빈 결과로 먼저 등록해
// 단독 cores 쿼리와 구분한다.
func TestPodDetail_NoLimitCpuCores(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_pod_info", sample(1,
			"namespace", "kube-system", "pod", "kube-proxy-x", "uid", "u-9", "node", "worker1")).
		on(`) / sum(kube_pod_container_resource_limits{namespace="kube-system", pod="kube-proxy-x", resource="cpu"})`).
		on(`) / sum(kube_pod_container_resource_limits{namespace="kube-system", pod="kube-proxy-x", resource="memory"})`).
		on("container_cpu_usage_seconds_total", sample(0.042)).
		on("sum(container_memory_working_set_bytes", sample(6.5e7))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPodDetail(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pod/kube-system/kube-proxy-x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp PodDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Vitals.CPUPercent != nil || resp.Vitals.MemoryPercent != nil {
		t.Errorf("percent=%+v want 생략 (limit 미설정)", resp.Vitals)
	}
	if resp.Vitals.CPUUsageCores == nil || *resp.Vitals.CPUUsageCores != 0.042 {
		t.Errorf("cpu_usage_cores=%v want 0.042 (limit 무관 산출)", resp.Vitals.CPUUsageCores)
	}
	if resp.Vitals.MemoryWorkingSetBytes == nil || *resp.Vitals.MemoryWorkingSetBytes != 6.5e7 {
		t.Errorf("working_set=%v want 6.5e7 (절대값 대칭)", resp.Vitals.MemoryWorkingSetBytes)
	}
	if resp.Cpu.LimitCores != nil {
		t.Errorf("limit_cores=%v want 생략", resp.Cpu.LimitCores)
	}
}

// TestPodDetail_LimitlessDimensions 는 #378 회귀 가드다. cpu·memory limit 이 없어 두 pressure score 와
// limit 대비 percent 를 산출할 수 없는 pod 는 health 에 network 만 담기고 cpu·memory 는 생략된다.
// node/pods 의 partial severity 와 규약을 통일하기 위해, "정상이라 생략" 이 아니라 "limit 없어 측정
// 불가" 임을 summary 에 명시한다. 이로써 node/pods (severity=partial) 와 pod-detail (health 생략 +
// summary 사유) 가 같은 pod 를 정합하게 표현한다.
func TestPodDetail_LimitlessDimensions(t *testing.T) {
	q := (&fakeQuerier{}).
		// percent 나눗셈 조각을 먼저 등록해 limit 없는 pod 의 cpu·memory percent 를 빈 결과로 만든다.
		on("/ sum(kube_pod_container_resource_limits").
		on("kube_pod_info", sample(1, "namespace", "mon", "pod", "prometheus-0", "uid", "u-9", "node", "n1", "pod_ip", "10.0.0.9")).
		on("pod:network_pressure_score:5m", sample(0.0002, "src_namespace", "mon", "src_pod", "prometheus-0")).
		on("sum(container_memory_working_set_bytes", sample(5e8)).
		on("container_cpu_usage_seconds_total", sample(0.3))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPodDetail(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pod/mon/prometheus-0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp PodDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// health 는 network 만 담기고 cpu·memory 는 생략된다.
	if v, ok := resp.Health["network"]; !ok || v < 0.999 || v > 1.0 {
		t.Errorf("health network=%v want ~0.9998 (1-0.0002)", resp.Health)
	}
	if _, ok := resp.Health["cpu"]; ok {
		t.Errorf("health cpu 존재 want 생략 (limit 없어 score 미산출)")
	}
	if _, ok := resp.Health["memory"]; ok {
		t.Errorf("health memory 존재 want 생략 (limit 없어 score 미산출)")
	}
	// percent 는 limit 없어 생략, 절대량 (cores) 은 존재.
	if resp.Vitals.CPUPercent != nil || resp.Vitals.MemoryPercent != nil {
		t.Errorf("percent=%+v want 생략 (limit 없음)", resp.Vitals)
	}
	// #378 node/pods 와 동일한 additive 구조 필드로 측정 불가 차원을 노출한다 (두 API 대칭).
	if len(resp.UnmeasuredDimensions) != 2 || resp.UnmeasuredDimensions[0] != "cpu" || resp.UnmeasuredDimensions[1] != "memory" {
		t.Errorf("unmeasured_dimensions=%v want [cpu memory]", resp.UnmeasuredDimensions)
	}
	// summary 가 limit 없어 측정 불가한 차원을 명시해 node/pods 의 결측 차원 노출과 정합.
	if !strings.Contains(resp.Summary, "limit 없어 pressure 측정 불가") {
		t.Errorf("summary=%q want cpu·memory 측정 불가 사유", resp.Summary)
	}
	if !strings.Contains(resp.Summary, "cpu") || !strings.Contains(resp.Summary, "memory") {
		t.Errorf("summary=%q want cpu·memory 차원 명시", resp.Summary)
	}
}
