package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"netobs/internal/apicommon"
)

// PlaybooksHandler 는 #238 의 원인별 대응 안내 API 다. gpu-idle cause 와 drop stage, 이벤트
// dimension, 주요 alertname 별로 원인 설명과 확인 절차 (API 경로), 권고 조치를 정적 카탈로그로
// 노출한다. incidents → at 재구성 → rca 로 이어진 사건 여정의 마지막 "무엇을 하라" 계층이며,
// docs/observability/ triage 문서의 대응 지식을 구조화한 단일 진실이다.
type PlaybooksHandler struct{}

// NewPlaybooksHandler 는 정적 카탈로그만 서빙하므로 의존성이 없다.
func NewPlaybooksHandler() *PlaybooksHandler {
	return &PlaybooksHandler{}
}

// Register 는 /api/v1/playbooks 라우트를 mux 에 등록한다.
func (h *PlaybooksHandler) Register(mux *http.ServeMux) {
	mux.Handle("/api/v1/playbooks", apicommon.Chain(
		http.HandlerFunc(h.GetPlaybooks),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
}

// PlaybooksResponse 는 GET /api/v1/playbooks 의 typed 응답이다.
type PlaybooksResponse struct {
	GeneratedAt string     `json:"generated_at"`
	Playbooks   []Playbook `json:"playbooks"`
	Summary     string     `json:"summary"`
}

// Playbook 은 한 원인 식별자의 대응 안내다. Cause 가 정식 식별자이고 Aliases 는 같은 항목으로
// 조회되는 별칭 (대응 alertname) 이다. cause 쿼리 파라미터는 양쪽 모두에 매칭된다.
type Playbook struct {
	Cause       string          `json:"cause"`
	Kind        string          `json:"kind"`
	Aliases     []string        `json:"aliases,omitempty"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Checks      []PlaybookCheck `json:"checks"`
	Actions     []string        `json:"actions"`
}

// PlaybookCheck 는 확인 절차 한 단계다. API 는 correlation-exporter 의 경로이며, 요청에 at 파라미터
// 를 주면 시점 지정 조회를 지원하는 경로에 한해 at 이 그대로 이어 붙는다.
type PlaybookCheck struct {
	Description string `json:"description"`
	API         string `json:"api"`
}

// playbookEntry 는 카탈로그 내부 표현이다. atCapable 은 해당 API 가 at 파라미터 (#235) 를 지원하는
// 경로인지 표시하며, 응답 생성 시 at 부착 여부를 결정한다.
type playbookEntry struct {
	cause       string
	kind        string
	aliases     []string
	title       string
	description string
	checks      []playbookCheckEntry
	actions     []string
}

type playbookCheckEntry struct {
	description string
	api         string
	atCapable   bool
}

// GetPlaybooks godoc
// @Summary      원인별 대응 안내 카탈로그
// @Description  gpu-idle cause 와 drop stage, 이벤트 dimension, 주요 alertname 별 대응 안내 (원인 설명, 확인 절차 API 경로, 권고 조치) 를 정적 카탈로그로 돌려준다. cause 파라미터로 단일 항목을 조회하며 GPUIdleWith* 같은 alertname 별칭으로도 매칭된다. at 파라미터를 주면 확인 절차의 API 링크 중 시점 지정 조회를 지원하는 경로에 at 이 이어 붙어 사건 시점 재구성과 결합된다.
// @Tags         playbook
// @Produce      json
// @Param        cause  query  string  false  "원인 식별자 (예: network_pressure, egress_qdisc, gpu, NetObsDropBurst). 미등록 식별자는 404"
// @Param        at     query  string  false  "확인 절차 링크에 결합할 평가 시점 (RFC3339 또는 unix seconds)"
// @Success      200  {object}  PlaybooksResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      404  {object}  apicommon.ErrorBody
// @Router       /api/v1/playbooks [get]
func (h *PlaybooksHandler) GetPlaybooks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	at := ""
	if raw := strings.TrimSpace(q.Get("at")); raw != "" {
		t, err := parseAtValue(raw)
		if err != nil {
			apicommon.WriteError(w, http.StatusBadRequest, "invalid_at", "at 파싱 실패 (RFC3339 또는 unix seconds): "+err.Error())
			return
		}
		at = t.Format(time.RFC3339)
	}

	entries := playbookCatalog
	if cause := strings.TrimSpace(q.Get("cause")); cause != "" {
		e := findPlaybook(cause)
		if e == nil {
			apicommon.WriteError(w, http.StatusNotFound, "unknown_cause", "미등록 원인 식별자: "+cause)
			return
		}
		entries = []playbookEntry{*e}
	}

	resp := PlaybooksResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Playbooks:   make([]Playbook, 0, len(entries)),
	}
	for _, e := range entries {
		resp.Playbooks = append(resp.Playbooks, renderPlaybook(e, at))
	}
	resp.Summary = fmt.Sprintf("대응 안내 %d개 (gpu-idle cause / drop stage / dimension / alert)", len(resp.Playbooks))
	apicommon.WriteJSON(w, resp)
}

// findPlaybook 은 cause 또는 alias 정확 일치로 항목을 찾는다. 식별자는 내부 enum 과 alertname
// 상수라 대소문자 정규화 없이 정확 일치만 지원한다.
func findPlaybook(cause string) *playbookEntry {
	for i := range playbookCatalog {
		e := &playbookCatalog[i]
		if e.cause == cause {
			return e
		}
		for _, a := range e.aliases {
			if a == cause {
				return e
			}
		}
	}
	return nil
}

// renderPlaybook 은 내부 표현을 응답 형태로 옮긴다. at 이 주어지면 시점 지정 조회를 지원하는
// 경로에만 at 을 이어 붙인다.
func renderPlaybook(e playbookEntry, at string) Playbook {
	checks := make([]PlaybookCheck, 0, len(e.checks))
	for _, c := range e.checks {
		api := c.api
		if at != "" && c.atCapable {
			api = withAt(api, at)
		}
		checks = append(checks, PlaybookCheck{Description: c.description, API: api})
	}
	return Playbook{
		Cause: e.cause, Kind: e.kind, Aliases: e.aliases,
		Title: e.title, Description: e.description,
		Checks: checks, Actions: e.actions,
	}
}

// playbookCatalog 는 대응 안내의 단일 진실이다. docs/observability/ triage 문서와 alert annotation
// 의 지식을 구조화했으며, 항목 추가 시 문서가 아닌 본 카탈로그를 갱신한다. kind 는 4종이다:
// gpu_idle_cause (gpu_idle_cause_weight:5m 의 cause enum), drop_stage (netobs drop_stage 라벨),
// dimension (events / pressure 의 4 차원), alert (cause 로 흡수되지 않는 독립 alertname).
var playbookCatalog = []playbookEntry{
	// ---- gpu_idle_cause 9종 ----
	{
		cause: "cpu_throttle", kind: "gpu_idle_cause",
		aliases: []string{"GPUIdleWithCPUThrottle"},
		title:   "CPU throttle 로 인한 GPU 유휴",
		description: "GPU 워크로드의 host 측 스레드 (데이터 로더, kernel launch) 가 CFS quota throttle 에 걸려 GPU 에 일을 공급하지 못하는 상태다. " +
			"pod:cpu_throttle_score:5m 이 base 신호다.",
		checks: []playbookCheckEntry{
			{description: "gpu-idle 원인 가중치에서 cpu_throttle 이 dominant 인지와 victim pod 확인", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "cpu 차원 pod 압박 랭킹에서 throttle 이 심한 pod 식별", api: "/api/v1/pressure?dimension=cpu&scope=pod", atCapable: true},
			{description: "victim pod 의 noisy neighbor 상관 확인 (동일 노드 CPU 경합원)", api: "/api/v1/noisy-neighbor"},
		},
		actions: []string{
			"victim pod 의 CPU limit 상향 또는 requests 와 limits 정합 조정",
			"동일 노드의 CPU 소모 큰 이웃 워크로드를 다른 노드로 재배치",
			"GPU 워크로드에 CPU 를 보장하는 Guaranteed QoS (requests=limits) 적용 검토",
		},
	},
	{
		cause: "memory_pressure", kind: "gpu_idle_cause",
		aliases: []string{"GPUIdleWithMemoryPressure"},
		title:   "메모리 압박으로 인한 GPU 유휴",
		description: "컨테이너 working set 이 memory limit 에 근접해 reclaim 과 allocation stall 이 GPU 파이프라인을 막는 상태다. " +
			"pod:memory_pressure_score:5m 이 base 신호다.",
		checks: []playbookCheckEntry{
			{description: "gpu-idle 원인 가중치에서 memory_pressure 가 dominant 인지 확인", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "메모리 병목 합성 뷰에서 working set 대비 limit 비율과 psi 확인", api: "/api/v1/memory", atCapable: true},
			{description: "memory 차원 pod 압박 랭킹에서 한계 근접 pod 식별", api: "/api/v1/pressure?dimension=memory&scope=pod", atCapable: true},
		},
		actions: []string{
			"victim pod 의 memory limit 상향 또는 배치 크기 등 워크로드 메모리 사용량 축소",
			"동일 노드 이웃의 메모리 과점유 확인 후 재배치",
			"OOMKill 이력이 있으면 재발 방지를 위해 limit 여유분을 관측된 peak 이상으로 확보",
		},
	},
	{
		cause: "network_pressure", kind: "gpu_idle_cause",
		aliases: []string{"GPUIdleWithNetworkPressure"},
		title:   "네트워크 압박으로 인한 GPU 유휴",
		description: "GPU 워크로드가 의존하는 pod 네트워크 I/O 가 throughput saturation 또는 재전송 급증으로 지연되어 GPU 가 데이터를 기다리는 상태다. " +
			"pod:network_throughput_score:5m 과 pod:network_retrans_score:5m 이 base 신호다.",
		checks: []playbookCheckEntry{
			{description: "gpu-idle 원인 가중치에서 network_pressure 가 dominant 인지와 victim pod 확인", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "victim pod 의 RX/TX 대역폭이 한계에 붙어 있는지 확인", api: "/api/v1/bandwidth", atCapable: true},
			{description: "drop 과 재전송 발생 여부와 stage 확인", api: "/api/v1/drops", atCapable: true},
			{description: "지연 단계 분해에서 병목 단계 (ack_wait 등) 확인", api: "/api/v1/latency-breakdown?scope=pod", atCapable: true},
		},
		actions: []string{
			"데이터 공급 경로 (스토리지, 피처 서버) 의 대역폭 병목 해소 또는 데이터 로컬리티 개선",
			"재전송 비율이 높으면 drop stage 안내에 따라 drop 원인 먼저 해소",
			"네트워크 집약 이웃 워크로드와 GPU 워크로드의 노드 분리",
		},
	},
	{
		cause: "pcie_saturation", kind: "gpu_idle_cause",
		aliases: []string{"GPUIdleWithPCIeSaturation"},
		title:   "PCIe 대역폭 포화로 인한 GPU 유휴",
		description: "host 와 GPU 간 PCIe link 전송이 포화되어 데이터 복사가 연산을 따라가지 못하는 상태다. " +
			"node:gpu_pcie_saturation_score:5m 이 base 신호다.",
		checks: []playbookCheckEntry{
			{description: "gpu-idle 원인 가중치에서 pcie_saturation 이 dominant 인지 확인", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "GPU 현황에서 PCIe 처리량과 사용률의 괴리 확인", api: "/api/v1/gpu-status", atCapable: true},
		},
		actions: []string{
			"host-device 복사 축소 (pinned memory, 배치 전송, 데이터 전처리의 GPU 이전) 검토",
			"동일 PCIe 스위치를 공유하는 장치 (NIC, NVMe) 와의 대역폭 경합 점검",
			"PCIe link generation/width 가 스펙보다 낮게 협상되었는지 하드웨어 점검",
		},
	},
	{
		cause: "host_compute_stall", kind: "gpu_idle_cause",
		aliases: []string{"GPUIdleWithHostComputeStall"},
		title:   "host 연산 지연으로 인한 GPU 유휴",
		description: "CUDA kernel launch 나 host 측 전처리가 밀려 GPU 에 커널이 공급되지 않는 상태다. throttle 없이도 host 연산 자체가 병목일 때 나타나며 " +
			"pod:host_compute_stall_score:5m 이 base 신호다.",
		checks: []playbookCheckEntry{
			{description: "gpu-idle 원인 가중치에서 host_compute_stall 이 dominant 인지 확인", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "GPU 현황에서 kernel launch 대비 사용률 확인", api: "/api/v1/gpu-status", atCapable: true},
			{description: "cpu 차원 압박과의 동반 여부 확인 (throttle 이면 cpu_throttle 안내로 전환)", api: "/api/v1/pressure?dimension=cpu&scope=pod", atCapable: true},
		},
		actions: []string{
			"데이터 로더 병렬도 (num_workers) 와 전처리 파이프라인 최적화",
			"CUDA stream 활용과 비동기 복사로 launch 공백 축소",
			"host 연산의 GPU 이전 (DALI 등 GPU 전처리) 검토",
		},
	},
	{
		cause: "dcgm_pcie_replay", kind: "gpu_idle_cause",
		aliases: []string{"GPUIdleWithDCGMPCIeReplay"},
		title:   "PCIe replay 증가로 인한 GPU 유휴",
		description: "PCIe link 의 전송 오류 재시도 (replay) 카운터가 증가하는 상태로, 신호 무결성 저하나 하드웨어 이상 징후다. " +
			"datacenter GPU 의 DCGM 카운터 기반 신호라 소비자용 GPU 환경에서는 관측되지 않을 수 있다.",
		checks: []playbookCheckEntry{
			{description: "gpu-idle 원인 가중치에서 dcgm_pcie_replay 가 dominant 인지 확인", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "GPU 현황에서 해당 GPU 의 상태 지표 확인", api: "/api/v1/gpu-status", atCapable: true},
		},
		actions: []string{
			"replay 가 지속 증가하면 GPU 재장착 또는 슬롯 교체 등 하드웨어 점검",
			"라이저 카드나 케이블 사용 시 신호 품질 점검",
			"해당 노드의 GPU 워크로드를 다른 노드로 우선 대피",
		},
	},
	{
		cause: "nccl_collective_stall", kind: "gpu_idle_cause",
		aliases: []string{"GPUIdleWithNCCLCollectiveStall"},
		title:   "NCCL collective 대기로 인한 GPU 유휴",
		description: "분산 학습의 collective 연산 (all-reduce 등) 에서 참여 rank 간 동기화 대기가 길어져 GPU 가 통신을 기다리는 상태다. " +
			"가장 느린 rank 하나가 전체를 멈추므로 straggler 식별이 핵심이다.",
		checks: []playbookCheckEntry{
			{description: "gpu-idle 원인 가중치에서 nccl_collective_stall 이 dominant 인지 확인", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "rank 간 통신 경로의 drop 과 재전송 확인", api: "/api/v1/drops", atCapable: true},
			{description: "노드 간 간섭 전파로 특정 rank 노드의 straggler 여부 확인", api: "/api/v1/cross-node-interference"},
		},
		actions: []string{
			"straggler rank 의 노드에서 다른 원인 (cpu_throttle, network_pressure) 안내를 교차 적용",
			"NCCL 통신 경로 (IB/RoCE/TCP) 설정과 토폴로지 정합 점검",
			"rank 배치를 균질한 노드 셋으로 재구성",
		},
	},
	{
		cause: "thermal", kind: "gpu_idle_cause",
		aliases: []string{"GPUIdleWithThermal", "GPUObsThermalThrottleSustained"},
		title:   "열 스로틀로 인한 GPU 성능 저하",
		description: "GPU 온도가 스로틀 임계에 도달해 클럭이 강제로 내려간 상태다. 사용률이 높아 보여도 실효 성능이 떨어지며, 지속되면 유휴 구간이 늘어난다.",
		checks: []playbookCheckEntry{
			{description: "GPU 현황에서 온도와 스로틀 사유, 전력 확인", api: "/api/v1/gpu-status", atCapable: true},
			{description: "gpu-idle 원인 가중치에서 thermal 기여 확인", api: "/api/v1/gpu-idle", atCapable: true},
		},
		actions: []string{
			"섀시 냉각 (팬 곡선, 흡배기) 점검과 먼지 청소",
			"power limit 하향으로 발열 자체를 줄여 지속 클럭 확보",
			"랙 내 열 밀집 시 GPU 노드 간 워크로드 분산",
		},
	},
	{
		cause: "cgroup_contention", kind: "gpu_idle_cause",
		title: "cgroup CPU 경합으로 인한 GPU 유휴",
		description: "quota throttle 없이도 동일 노드의 다른 cgroup 과 CPU 시간을 경합해 GPU 공급 스레드의 실행이 지연되는 상태다. " +
			"cpu_throttle 과 달리 limit 이 아니라 이웃과의 경쟁이 원인이다.",
		checks: []playbookCheckEntry{
			{description: "gpu-idle 원인 가중치에서 cgroup_contention 이 dominant 인지 확인", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "victim pod 의 noisy neighbor 상관에서 CPU 경합원 식별", api: "/api/v1/noisy-neighbor"},
			{description: "cpu 차원 pod 압박 랭킹에서 경합 pod 확인", api: "/api/v1/pressure?dimension=cpu&scope=pod", atCapable: true},
		},
		actions: []string{
			"경합원 워크로드의 재배치 또는 CPU requests 명시로 스케줄러의 과적재 방지",
			"GPU 워크로드에 cpu-manager static policy 등 CPU 전용 할당 검토",
			"노드의 allocatable 대비 requests 합계를 점검해 과커밋 해소",
		},
	},

	// ---- drop_stage 10종 ----
	{
		cause: "ingress_tc", kind: "drop_stage",
		title: "TC ingress 단계 drop",
		description: "수신 경로의 TC (traffic control) ingress hook 에서 패킷이 버려지는 상태다. CNI 의 필터 체인이나 정책 프로그램이 개입하는 지점이다.",
		checks: []playbookCheckEntry{
			{description: "해당 stage 의 drop reason 과 category, 귀속 pod, kernel stack 확인", api: "/api/v1/drops", atCapable: true},
			{description: "drop 이 걸린 flow 의 양 끝 pod 식별", api: "/api/v1/flows"},
		},
		actions: []string{
			"CNI (Cilium 등) 의 정책과 필터 설정에서 해당 flow 허용 여부 확인",
			"drop reason 이 정책성 (policy) 이면 NetworkPolicy 규칙을 의도와 대조",
			"의도된 차단이면 워크로드의 목적지 설정 오류를 수정",
		},
	},
	{
		cause: "egress_tc", kind: "drop_stage",
		title: "TC egress 단계 drop",
		description: "송신 경로의 TC egress hook 에서 패킷이 버려지는 상태다. egress 정책이나 대역폭 제한 프로그램이 개입하는 지점이다.",
		checks: []playbookCheckEntry{
			{description: "해당 stage 의 drop reason 과 귀속 pod 확인", api: "/api/v1/drops", atCapable: true},
			{description: "송신 pod 의 대역폭이 제한에 붙어 있는지 확인", api: "/api/v1/bandwidth", atCapable: true},
		},
		actions: []string{
			"egress NetworkPolicy 와 대역폭 annotation (kubernetes.io/egress-bandwidth) 확인",
			"의도된 제한이면 제한값 상향 또는 트래픽 분산",
		},
	},
	{
		cause: "egress_qdisc", kind: "drop_stage",
		title: "송신 큐 (qdisc) 포화 drop",
		description: "송신 qdisc 큐가 가득 차 패킷이 버려지는 상태다. 송신량이 링크 또는 큐 용량을 초과하는 국면에서 나타나며 버스트 트래픽이 흔한 원인이다.",
		checks: []playbookCheckEntry{
			{description: "drop 시점과 rate, 귀속 pod 확인", api: "/api/v1/drops", atCapable: true},
			{description: "송신 pod 의 TX 대역폭 추이로 버스트 여부 확인", api: "/api/v1/bandwidth", atCapable: true},
			{description: "network 차원 압박 랭킹에서 동시 송신 경합 pod 확인", api: "/api/v1/pressure?dimension=network&scope=pod", atCapable: true},
		},
		actions: []string{
			"송신 버스트 완화 (송신 측 pacing, 배치 크기 조정)",
			"qdisc 큐 길이 (txqueuelen) 상향 검토",
			"동일 노드의 대량 송신 워크로드 분산",
		},
	},
	{
		cause: "recv_reorder", kind: "drop_stage",
		title: "수신 재조립 (OFO) 단계 drop",
		description: "TCP out-of-order 큐가 한계에 달해 순서 어긋난 세그먼트가 버려지는 상태다. 경로상 재정렬이나 앞선 구간의 drop 이 유발한다.",
		checks: []playbookCheckEntry{
			{description: "동반된 다른 stage 의 drop (원인 구간) 확인", api: "/api/v1/drops", atCapable: true},
			{description: "지연 단계 분해에서 재전송 대기 (ack_wait) 비중 확인", api: "/api/v1/latency-breakdown?scope=pod", atCapable: true},
		},
		actions: []string{
			"경로 앞 구간의 drop (qdisc 포화 등) 을 먼저 해소",
			"수신 버퍼 (tcp_rmem) 와 OFO 큐 한계 점검",
		},
	},
	{
		cause: "recv_tcp", kind: "drop_stage",
		title: "TCP 수신 처리 단계 drop",
		description: "TCP 수신 처리 (윈도, 시퀀스 검증, 버퍼) 에서 세그먼트가 버려지는 상태다. 수신 버퍼 고갈이나 비정상 세그먼트가 원인이다.",
		checks: []playbookCheckEntry{
			{description: "drop reason 세부 (버퍼/윈도/시퀀스) 와 귀속 pod 확인", api: "/api/v1/drops", atCapable: true},
			{description: "수신 pod 의 메모리 압박 동반 여부 확인 (버퍼 고갈 연관)", api: "/api/v1/memory", atCapable: true},
		},
		actions: []string{
			"수신 애플리케이션의 소비 속도 점검 (read 지연이 버퍼를 채움)",
			"수신 버퍼 한계 (tcp_rmem, rmem_max) 상향 검토",
		},
	},
	{
		cause: "socket", kind: "drop_stage",
		title: "소켓 수용 한계 drop",
		description: "소켓 수신 버퍼 초과나 accept queue overflow 로 패킷이 버려지는 상태다. 애플리케이션이 연결 또는 데이터를 제때 수용하지 못하는 신호다.",
		checks: []playbookCheckEntry{
			{description: "drop reason (버퍼/listen overflow) 과 귀속 pod 확인", api: "/api/v1/drops", atCapable: true},
			{description: "해당 pod 의 CPU throttle 동반 여부 확인 (처리 지연 원인)", api: "/api/v1/pressure?dimension=cpu&scope=pod", atCapable: true},
		},
		actions: []string{
			"accept queue overflow 면 backlog (somaxconn, listen backlog) 상향과 accept 루프 처리량 점검",
			"애플리케이션 처리 지연 (GC, lock, CPU 부족) 해소",
			"수요 초과가 지속되면 replica 수평 확장",
		},
	},
	{
		cause: "routing", kind: "drop_stage",
		title: "라우팅 단계 drop",
		description: "목적지 경로 부재나 라우팅 실패로 패킷이 버려지는 상태다. CNI 라우트 테이블 불일치나 삭제된 endpoint 로의 잔류 트래픽이 흔한 원인이다.",
		checks: []playbookCheckEntry{
			{description: "drop 대상 flow 의 목적지 (사라진 pod IP 여부) 확인", api: "/api/v1/drops", atCapable: true},
			{description: "현재 pod 인벤토리와 대조해 목적지 실존 여부 확인", api: "/api/v1/pods"},
		},
		actions: []string{
			"목적지가 삭제된 pod IP 면 클라이언트의 stale 연결/캐시 (DNS TTL) 정리",
			"노드 라우트 테이블과 CNI 상태 정합 점검 (CNI 재시작 이력 확인)",
		},
	},
	{
		cause: "ingress_early", kind: "drop_stage",
		title: "수신 초기 단계 drop",
		description: "프로토콜 처리 이전의 이른 수신 단계 (드라이버-스택 경계) 에서 패킷이 버려지는 상태다. 수신 부하 폭주나 비정상 프레임이 원인일 수 있다.",
		checks: []playbookCheckEntry{
			{description: "drop rate 추이와 노드 귀속 확인", api: "/api/v1/drops", atCapable: true},
			{description: "해당 노드의 수신 대역폭 폭주 여부 확인", api: "/api/v1/bandwidth", atCapable: true},
		},
		actions: []string{
			"수신 부하 폭주면 유입원 식별 후 rate limit 또는 분산",
			"드라이버 ring buffer 크기와 인터럽트 (RSS/RPS) 분배 점검",
		},
	},
	{
		cause: "protocol", kind: "drop_stage",
		title: "프로토콜 검증 단계 drop",
		description: "프로토콜 계층의 검증 실패 (비정상 헤더, 지원하지 않는 프로토콜, checksum 오류 등) 로 패킷이 버려지는 상태다.",
		checks: []playbookCheckEntry{
			{description: "drop reason 과 category (checksum/protocol) 확인", api: "/api/v1/drops", atCapable: true},
		},
		actions: []string{
			"checksum 오류가 지속되면 NIC offload (tx/rx checksum) 설정과 하드웨어 점검",
			"특정 flow 에 집중되면 송신 측 스택/미들웨어의 패킷 조작 여부 확인",
		},
	},
	{
		cause: "unknown", kind: "drop_stage",
		title: "미분류 단계 drop",
		description: "drop reason 이 알려진 stage 분류에 매칭되지 않은 상태다. 신규 커널 reason 이거나 reason 자체가 NOT_SPECIFIED 인 경우다.",
		checks: []playbookCheckEntry{
			{description: "raw drop reason 과 kernel stack 으로 실제 drop 지점 확인", api: "/api/v1/drops", atCapable: true},
		},
		actions: []string{
			"kernel stack 의 함수명으로 drop 지점을 수동 판별 후 해당 stage 안내 적용",
			"반복 관측되는 신규 reason 은 stage 분류 (internal/netobs/drop) 에 추가",
		},
	},

	// ---- dimension 4종 ----
	{
		cause: "cpu", kind: "dimension",
		title: "CPU 차원 이상 이벤트",
		description: "CPU throttle 이나 노드 CPU 압박이 임계를 넘은 이벤트다. GPU 워크로드가 있으면 GPU 유휴로 전이될 수 있다.",
		checks: []playbookCheckEntry{
			{description: "cpu 차원 pod 압박 랭킹에서 원인 pod 식별", api: "/api/v1/pressure?dimension=cpu&scope=pod", atCapable: true},
			{description: "victim-suspect 상관에서 간섭 관계 확인", api: "/api/v1/noisy-neighbor"},
			{description: "GPU 노드면 gpu-idle 전이 여부 확인", api: "/api/v1/gpu-idle", atCapable: true},
		},
		actions: []string{
			"throttle 이 원인이면 limit 조정, 경합이 원인이면 워크로드 재배치",
			"발화 이력 (incidents) 으로 재발 주기를 확인해 상시성 여부 판단",
		},
	},
	{
		cause: "gpu", kind: "dimension",
		title: "GPU 차원 이상 이벤트",
		description: "GPU 사용률 이상이나 유휴 원인 신호가 임계를 넘은 이벤트다. 원인 식별은 gpu-idle 의 cause 가중치가 진입점이다.",
		checks: []playbookCheckEntry{
			{description: "gpu-idle 원인 가중치에서 dominant cause 식별", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "GPU 현황 (사용률, 메모리, 전력, 온도) 확인", api: "/api/v1/gpu-status", atCapable: true},
		},
		actions: []string{
			"dominant cause 의 gpu_idle_cause 안내 (cpu_throttle 등) 를 적용",
			"cause 미식별 (N/A) 이면 워크로드 자체의 유휴 (배치 대기 등) 를 의심",
		},
	},
	{
		cause: "memory", kind: "dimension",
		title: "메모리 차원 이상 이벤트",
		description: "working set 의 limit 근접이나 psi 압박이 임계를 넘은 이벤트다. 방치 시 OOMKill 로 이어질 수 있다.",
		checks: []playbookCheckEntry{
			{description: "메모리 병목 합성 뷰에서 원인 pod 와 psi 확인", api: "/api/v1/memory", atCapable: true},
			{description: "memory 차원 pod 압박 랭킹 확인", api: "/api/v1/pressure?dimension=memory&scope=pod", atCapable: true},
		},
		actions: []string{
			"limit 근접 pod 의 limit 상향 또는 메모리 사용량 축소",
			"노드 단위 압박이면 과커밋 해소 (requests 합계 점검)",
		},
	},
	{
		cause: "network", kind: "dimension",
		title: "네트워크 차원 이상 이벤트",
		description: "drop rate 나 throughput 압박, 재전송이 임계를 넘은 이벤트다. drop 은 stage 별 안내가, 지연은 단계 분해가 진입점이다.",
		checks: []playbookCheckEntry{
			{description: "drop 발생 여부와 stage 확인 (있으면 해당 drop_stage 안내로)", api: "/api/v1/drops", atCapable: true},
			{description: "지연 단계 분해에서 병목 단계 확인", api: "/api/v1/latency-breakdown?scope=pod", atCapable: true},
			{description: "대역폭 포화 여부 확인", api: "/api/v1/bandwidth", atCapable: true},
		},
		actions: []string{
			"drop 이 있으면 stage 별 안내를 먼저 적용 (drop 이 지연과 재전송의 상류 원인)",
			"포화면 트래픽 분산 또는 대역폭 경합 워크로드 분리",
		},
	},

	// ---- alert 3종 (cause 로 흡수되지 않는 독립 alertname) ----
	{
		cause: "CorrelationStrongNoisyNeighbor", kind: "alert",
		title: "강한 noisy neighbor 상관 감지",
		description: "victim pod 의 성능 저하와 suspect pod 의 자원 사용 간 피어슨 상관이 임계를 넘어 지속되는 상태다. 동일 노드 자원 경합의 정량 신호다.",
		checks: []playbookCheckEntry{
			{description: "victim-suspect 쌍과 차원별 상관 점수 확인", api: "/api/v1/noisy-neighbor"},
			{description: "alert 의 RCA 요약 (지배 차원, 최우선 의심 pod) 확인", api: "/api/v1/rca?alert=CorrelationStrongNoisyNeighbor"},
			{description: "suspect 차원의 pod 압박 랭킹으로 정합 확인", api: "/api/v1/pressure?scope=pod", atCapable: true},
		},
		actions: []string{
			"suspect pod 의 재배치 (nodeSelector, anti-affinity) 로 victim 과 노드 분리",
			"suspect 의 requests 명시로 스케줄러가 경합을 반영하게 조정",
			"상관의 지배 차원 (dimension) 안내를 교차 적용",
		},
	},
	{
		cause: "NetObsDropBurst", kind: "alert",
		title: "특정 flow 의 drop burst",
		description: "단일 5-tuple flow 에서 drop 이 1분 이상 지속 폭주하는 상태다. 특정 통신 쌍에 국한된 문제라 flow 의 양 끝과 stage 식별이 우선이다.",
		checks: []playbookCheckEntry{
			{description: "burst flow 의 stage 와 reason, kernel stack 확인", api: "/api/v1/drops", atCapable: true},
			{description: "alert 의 RCA 요약 (핵심 flow 표현) 확인", api: "/api/v1/rca?alert=NetObsDropBurst"},
			{description: "발화 이력으로 burst 의 시작 시점 특정 후 at 으로 당시 상태 재구성", api: "/api/v1/incidents"},
		},
		actions: []string{
			"식별된 stage 의 drop_stage 안내를 적용",
			"특정 목적지로의 반복 burst 면 해당 endpoint 의 실존과 정책 허용 여부 확인",
		},
	},
	{
		cause: "GPUObsCudaStreamWaitHigh", kind: "alert",
		title: "CUDA stream 대기 비중 과다",
		description: "CUDA stream 의 동기화 대기가 실행 시간 대비 과도한 상태다. GPU 는 점유되어 보이나 실제로는 대기가 지배해 실효 처리량이 낮다.",
		checks: []playbookCheckEntry{
			{description: "GPU 현황에서 사용률과 kernel launch 추이 확인", api: "/api/v1/gpu-status", atCapable: true},
			{description: "gpu-idle 원인 가중치에서 host_compute_stall 동반 여부 확인", api: "/api/v1/gpu-idle", atCapable: true},
			{description: "alert 의 RCA 요약 확인", api: "/api/v1/rca?alert=GPUObsCudaStreamWaitHigh"},
		},
		actions: []string{
			"불필요한 stream 동기화 (synchronize, event wait) 축소와 비동기화",
			"단일 stream 직렬 실행이면 다중 stream 으로 겹침 (overlap) 확보",
			"host_compute_stall 동반 시 해당 안내를 교차 적용",
		},
	},
}
