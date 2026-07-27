package api

import (
	"regexp"
	"strings"
)

// incidentcatalog.go 는 #349 의 alert 설명 카탈로그다. Prometheus ALERTS 시계열은 rule 정의의
// annotations.summary/description 을 싣지 않으므로, incidents API 가 사람이 읽을 title 과 summary 를
// 내려면 alertname 을 코드 카탈로그로 매핑해야 한다. playbookCatalog / rcaCauseCatalog 와 동일한
// 관용구로, prometheus-rule.yaml 에 alert 를 추가하면 본 카탈로그도 갱신해야 하며 정합은
// incidentcatalog_test.go 의 rule↔카탈로그 키 차집합 검증이 CI 에서 강제한다.
//
// summary 템플릿은 항목의 labels 로 치환되는 {{key}} placeholder 만 쓴다. ALERTS 시계열은 rule
// annotation 의 $value 를 싣지 않으므로 수치는 담지 않고 label 로 식별 가능한 대상만 렌더한다.
// 미등록 alertname 은 title 에 alertname 을 그대로 쓰고 summary 를 생략해 graceful 처리한다.

type incidentInfo struct {
	title   string
	summary string
}

// incidentCatalog 는 alertname → (title, summary 템플릿) 매핑이다. 키 집합은 3 개 rule 파일
// (gpuobs / correlation / injector) 의 alert 집합과 정합해야 한다 (정합 테스트). kube-prometheus-stack
// 내장 alert (Watchdog / InfoInhibitor) 는 rule 파일 밖이라 카탈로그 대상이 아니며 미등록 graceful
// 경로로 처리된다.
var incidentCatalog = map[string]incidentInfo{
	// netobs (network observability) 계열
	"NetObsHighStageLatencyP99":    {"네트워크 stage 지연 급증", "{{node}}의 {{src_namespace}}/{{src_workload}} stage 지연 p99가 임계를 초과함"},
	"NetObsHighDropRate":           {"네트워크 drop rate 급증", "{{node}}에서 {{drop_category}} 계열 packet drop rate가 임계를 초과함"},
	"NetObsDropBurst":              {"5-tuple drop burst", "{{src_namespace}}/{{src_pod}} 연결에서 packet drop이 집중 발생함"},
	"NetObsHighRetransmissionRate": {"TCP 재전송률 급증", "{{node}}의 {{src_namespace}}/{{src_workload}} TCP 재전송률이 임계를 초과함"},
	"NetObsTcpCongestion":          {"수신 pod TCP 혼잡", "{{namespace}}/{{pod}}가 TCP 혼잡 상태로 수신 지연이 발생함"},

	// netobs / gpuobs agent self-health 계열
	"ObsAgentDown":                {"관측 에이전트 다운", "{{job}}가 {{node|instance}}에서 다운됨"},
	"ObsAgentInformerStale":       {"에이전트 informer 정체", "{{job}}의 kube informer가 {{node|instance}}에서 정체됨"},
	"NetObsBpfProgramUnavailable": {"BPF 프로브 미부착", "netobs BPF 프로브 {{symbol}}가 {{node|instance}}에 부착되지 않음"},
	"NetObsBpfAttachFailureHigh":  {"BPF attach 실패 급증", "netobs BPF 프로그램 {{program}}의 attach 실패가 {{node|instance}}에서 누적됨"},
	"NetObsAgentBpfDropsHigh":     {"ringbuf drop 급증", "netobs ringbuf drop이 {{node|instance}}에서 임계를 초과함"},
	"NetObsBpfMapUtilizationHigh": {"BPF 맵 사용률 임계 초과", "netobs BPF {{map}} 맵 사용률이 {{node|instance}}에서 임계에 근접함"},
	"GPUObsCudaSymbolUnavailable": {"CUDA 심볼 미부착", "gpuobs CUDA 심볼 {{symbol}}가 {{node|instance}}에 부착되지 않음"},
	"GPUObsAgentNvmlErrorsHigh":   {"NVML 호출 오류 급증", "gpuobs NVML 호출 {{call}}이 {{node|instance}}에서 {{error_code}}로 실패함"},

	// gpuobs (하드웨어 신호) 계열
	"GPUObsThrottleActive":           {"GPU throttle 활성", "{{node}}의 GPU가 {{reason}} 사유로 throttle 중임"},
	"GPUObsThermalHeadroomLow":       {"GPU 열 여유 부족", "{{node}}의 GPU 열 여유가 임계 이내로 좁혀짐"},
	"GPUObsPowerHeadroomLow":         {"GPU 전력 여유 부족", "{{node}}의 GPU 전력 여유가 임계 이내로 좁혀짐"},
	"GPUObsCudaStreamWaitHigh":       {"CUDA stream 동기 대기 급증", "{{node}}의 {{src_namespace}}/{{src_pod}} cuStreamSynchronize 지연이 임계를 초과함"},
	"GPUObsThermalThrottleSustained": {"GPU 열 throttle 지속", "{{node}}의 GPU가 열 throttle을 지속함"},

	// gpu-idle dominant cause 계열
	"GPUIdleWithPCIeSaturation":           {"GPU 유휴 (PCIe 포화)", "{{node}}에서 PCIe 포화로 GPU가 유휴 상태임"},
	"GPUIdleWithNetworkPressure":          {"GPU 유휴 (네트워크 압박)", "{{node}}의 {{src_namespace}}/{{src_pod}} 네트워크 압박으로 GPU가 유휴 상태임"},
	"GPUIdleWithCPUThrottle":              {"GPU 유휴 (CPU throttle)", "{{node}}의 {{src_namespace}}/{{src_pod}} CPU throttle로 GPU가 유휴 상태임"},
	"GPUIdleWithMemoryPressure":           {"GPU 유휴 (메모리 압박)", "{{node}}의 {{src_namespace}}/{{src_pod}} 메모리 압박으로 GPU가 유휴 상태임"},
	"GPUIdleWithHostComputeStall":         {"GPU 유휴 (host 연산 정체)", "{{node}}의 {{src_namespace}}/{{src_pod}} host 연산 정체로 GPU가 유휴 상태임"},
	"GPUIdleWithDCGMPCIeReplay":           {"GPU 유휴 (PCIe replay)", "{{node}}에서 PCIe replay 재시도로 GPU가 유휴 상태임"},
	"GPUIdleWithNCCLCollectiveStall":      {"GPU 유휴 (NCCL collective 정체)", "{{node}}에서 NCCL collective 동기 대기로 GPU가 유휴 상태임"},
	"GPUIdleWithThermal":                  {"GPU 유휴 (열 throttle)", "{{node}}에서 열 throttle로 GPU가 유휴 상태임"},
	"GPUIdleDominantCauseSwitch":          {"GPU 유휴 원인 불안정", "{{node}}의 GPU 유휴 dominant 원인이 짧은 시간에 반복 전환됨"},
	"GPUIdleDominantCauseAmbiguous":       {"GPU 유휴 원인 모호", "{{node}}의 GPU 유휴 dominant 원인 top1과 top2 격차가 작아 판정이 모호함"},
	"VictimGPUIdleDominantCauseAmbiguous": {"victim GPU 유휴 원인 모호", "{{victim_namespace}}/{{victim_pod}}의 GPU 유휴 dominant 원인 판정이 모호함"},

	// capacity anomaly / spike (z-score) 계열
	"GPUUtilAnomalyDetected":        {"GPU 사용률 이상", "GPU 사용률 용량 이상이 z-score 기준으로 감지됨"},
	"NetworkUtilAnomalyDetected":    {"네트워크 처리량 이상", "네트워크 처리량 용량 이상이 z-score 기준으로 감지됨"},
	"CPUThrottleAnomalyDetected":    {"CPU throttle 이상", "CPU throttle 용량 이상이 z-score 기준으로 감지됨"},
	"MemoryPressureAnomalyDetected": {"메모리 압박 이상", "메모리 압박 용량 이상이 z-score 기준으로 감지됨"},
	"GPUUtilSpikeDetected":          {"GPU 사용률 급변", "GPU 사용률 spike가 z-score 기준으로 감지됨"},
	"NetworkDropSpikeDetected":      {"네트워크 drop 급변", "네트워크 drop spike가 z-score 기준으로 감지됨"},
	"CPUThrottleSpikeDetected":      {"CPU throttle 급변", "CPU throttle spike가 z-score 기준으로 감지됨"},
	"MemoryPressureSpikeDetected":   {"메모리 압박 급변", "메모리 압박 spike가 z-score 기준으로 감지됨"},

	// correlation-exporter self-health 계열
	"CorrelationStrongNoisyNeighbor":     {"강한 noisy neighbor 간섭", "{{suspect_namespace}}/{{suspect_pod}}가 {{victim_namespace}}/{{victim_pod}}를 {{resource_dimension}} 자원 경합으로 간섭"},
	"CorrelationExporterStalled":         {"correlation-exporter 정체", "correlation-exporter가 10분 이상 reconcile하지 않음"},
	"CorrelationExporterReconcileErrors": {"correlation-exporter reconcile 오류", "correlation-exporter의 reconcile 오류가 누적됨"},

	// injector (부하 주입) 계열
	"InjectorActive":                  {"워크로드 injector 활성", "{{target_namespace}}/{{target_pod}} 대상으로 워크로드 injector가 활성 중임"},
	"BlastRadiusHigh":                 {"blast radius 과다", "{{target_namespace}}/{{target_pod}} 주입의 영향 범위가 과다함"},
	"InjectorStuck":                   {"injector 정체", "{{target_namespace}}/{{target_pod}} 대상 injector가 정체됨"},
	"LoadScenarioReconcilerStalled":   {"loadscenario 컨트롤러 정체", "loadscenario 컨트롤러가 5분 이상 reconcile하지 않음"},
	"LoadScenarioConsecutiveFailures": {"loadscenario 연속 실패", "{{namespace}}/{{loadscenario}}가 연속 실패함"},
}

// incidentPlaceholder 는 summary 템플릿의 {{key}} placeholder 를 매칭한다. key 는 파이프로 대안을
// 나열할 수 있어 ({{node|instance}}) 첫 존재 라벨을 고른다. ALERTS 시계열의 라벨셋이 rule
// annotation 의 $labels 참조와 항상 일치하지는 않아 (예: NetObsBpfMapUtilizationHigh 는 annotation 이
// instance 를 참조하나 실제 시리즈는 node 를 가짐), 위치 라벨은 대안 나열로 견고하게 고른다.
var incidentPlaceholder = regexp.MustCompile(`\{\{([\w|]+)\}\}`)

// incidentDescribe 는 alertname 과 항목 labels 로 title 과 렌더된 summary 를 돌려준다. 카탈로그
// 미등록 alertname 은 title 에 alertname 을 그대로 쓰고 summary 는 빈 문자열이다 (graceful). summary
// 의 {{key}} 는 labels[key] 로 치환하며, 파이프 대안 ({{a|b}}) 은 첫 존재 라벨을, 모두 비면
// "unknown" 을 채워 문장이 깨지지 않게 한다.
func incidentDescribe(alertname string, labels map[string]string) (title, summary string) {
	info, ok := incidentCatalog[alertname]
	if !ok {
		return alertname, ""
	}
	title = info.title
	summary = incidentPlaceholder.ReplaceAllStringFunc(info.summary, func(m string) string {
		spec := incidentPlaceholder.FindStringSubmatch(m)[1]
		for _, key := range strings.Split(spec, "|") {
			if v := labels[key]; v != "" {
				return v
			}
		}
		return "unknown"
	})
	return title, summary
}
