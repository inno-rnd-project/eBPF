package correlation

import "testing"

// victim 시계열 query (DefaultMetrics 에 들어가는 인라인 쿼리 형태와 동일 토큰).
const (
	throughputMetric = `sum by(node, src_namespace, src_pod, src_pod_uid) (rate(netobs_pod_bytes_total[5m]))`
	errorMetric      = `sum by(node, src_namespace, src_pod) (rate(netobs_drop_events_flow_total[5m]))`
	gpuMetric        = `avg by(node, src_namespace, src_pod, src_pod_uid) (pod:gpu_util_p95:5m)`
)

// TestClassifyVictimSignal 은 victim 토큰 분류와 suspect cause score 명과의 비충돌을 검증한다. 이슈
// #150 의 "dimension 분류 정합 유지" 핵심 회귀 가드다.
func TestClassifyVictimSignal(t *testing.T) {
	cases := []struct {
		metric string
		want   VictimSignal
	}{
		{latencyMetric, SignalLatency},
		{throughputMetric, SignalThroughput},
		{errorMetric, SignalError},
		{gpuMetric, SignalGPU},
		// suspect cause score 는 victim 이 아니어야 한다 (충돌 회피).
		{"pod:cpu_throttle_score:5m", SignalNone},
		{"pod:memory_pressure_score:5m", SignalNone},
		{"pod:network_throughput_score:5m", SignalNone},
		{"pod:network_retrans_score:5m", SignalNone},
		{"pod:host_compute_stall_score:5m", SignalNone},
		// #174 GPU suspect (gpu_memory_utilization_ratio) 는 pod:gpu_util 토큰을 포함하지 않아 victim 으로
		// 오분류되지 않고 suspect (SignalNone) 로 남아야 한다. GPU victim 추가의 핵심 비충돌 가드다.
		{"pod:gpu_memory_utilization_ratio:5m", SignalNone},
		{`avg by(node, src_namespace, src_pod, src_pod_uid) (pod:gpu_memory_utilization_ratio:5m)`, SignalNone},
		// #174 운영자가 ExtraMetrics 로 추가할 수 있는 커스텀 GPU suspect 는 gpu_util 부분 문자열을 포함
		// 해도 pod: 접두사가 없어 victim 으로 오분류되지 않고 suspect (SignalNone) 로 남아야 한다.
		{"container_gpu_utilization_percent", SignalNone},
		{`sum by(node) (rate(node_gpu_utilization[5m]))`, SignalNone},
		{"node:gpu_util_p95:5m", SignalNone},
		// 운영자가 ExtraMetrics 로 추가할 수 있는 커스텀 suspect 는 "bytes" / "drop" 일반 토큰을 포함해도
		// victim 으로 오분류되지 않아야 한다 (source 메트릭 이름 매칭으로 차단).
		{`container_network_receive_bytes_total`, SignalNone},
		{`container_memory_working_set_bytes`, SignalNone},
		{`rate(node_network_receive_drop_total[5m])`, SignalNone},
	}
	for _, c := range cases {
		if got := classifyVictimSignal(c.metric); got != c.want {
			t.Errorf("classifyVictimSignal(%q)=%q want %q", c.metric, got, c.want)
		}
	}
}

// TestDefaultConfig_HasMultiSignalVictims 는 DefaultMetrics 에 네 victim signal (latency / throughput
// / error / gpu) 이 각각 정확히 하나씩 포함되어 zero-config 에서도 다차원 victim 이 산출되는지 검증한다.
func TestDefaultConfig_HasMultiSignalVictims(t *testing.T) {
	counts := map[VictimSignal]int{}
	for _, m := range DefaultConfig().DefaultMetrics {
		counts[classifyVictimSignal(m)]++
	}
	for _, sig := range []VictimSignal{SignalLatency, SignalThroughput, SignalError, SignalGPU} {
		if counts[sig] != 1 {
			t.Errorf("DefaultMetrics 의 victim_signal=%s 개수=%d want 1", sig, counts[sig])
		}
	}
	// cause score 들은 victim 이 아니어야 한다 (SignalNone 다수).
	if counts[SignalNone] == 0 {
		t.Errorf("DefaultMetrics 에 suspect cause score (SignalNone) 가 없음")
	}
}

// TestSelectTopN_MultiSignalVictims 는 한 victim 이 latency / throughput / error / gpu 네 신호별로 독립
// 순위를 갖고 victim_signal 이 정확히 채워지는지 검증한다.
func TestSelectTopN_MultiSignalVictims(t *testing.T) {
	results := []CorrelationResult{
		makeResult("ns", "sus", "us", "pod:cpu_throttle_score:5m", "ns", "vic", "uv", latencyMetric, 0.9, 1, StatusOK),
		makeResult("ns", "sus", "us", "pod:cpu_throttle_score:5m", "ns", "vic", "uv", throughputMetric, 0.8, 1, StatusOK),
		makeResult("ns", "sus", "us", "pod:cpu_throttle_score:5m", "ns", "vic", "uv", errorMetric, 0.7, 1, StatusOK),
		makeResult("ns", "sus", "us", "pod:cpu_throttle_score:5m", "ns", "vic", "uv", gpuMetric, 0.6, 1, StatusOK),
	}
	got := SelectTopN(results, 10)
	if len(got) != 4 {
		t.Fatalf("len=%d want 4 (signal 별 독립 항목)", len(got))
	}
	bySignal := map[VictimSignal]NoisyNeighbor{}
	for _, n := range got {
		bySignal[n.VictimSignal] = n
		if n.Dimension != DimensionCPU {
			t.Errorf("signal=%s dimension=%s want cpu", n.VictimSignal, n.Dimension)
		}
		if n.Rank != 1 {
			t.Errorf("signal=%s rank=%d want 1 (각 그룹 단일 suspect)", n.VictimSignal, n.Rank)
		}
	}
	if bySignal[SignalLatency].Score != 0.9 || bySignal[SignalThroughput].Score != 0.8 ||
		bySignal[SignalError].Score != 0.7 || bySignal[SignalGPU].Score != 0.6 {
		t.Errorf("signal 별 score 불일치: %+v", bySignal)
	}
}

// TestSelectTopN_GPUVictimTopN 은 #174 의 수용 조건이다. GPU victim 신호 (pod:gpu_util_p95:5m) 를
// victim 으로 하는 페어가 (victim, gpu, dimension) 그룹에서 suspect 압박 score 내림차순 Top-N 으로
// 산출되고 victim_signal 이 gpu 로 채워지는지 입증한다. 네트워크 suspect 가 GPU victim 의 rank 1 이
// 되는 케이스 (네트워크 저하 → GPU 저하 경로) 를 포함한다.
func TestSelectTopN_GPUVictimTopN(t *testing.T) {
	results := []CorrelationResult{
		// 동일 GPU victim 에 대해 network suspect 가 cpu suspect 보다 강한 상관.
		makeResult("ns", "net", "un", "pod:network_throughput_score:5m", "ns", "gvic", "ug", gpuMetric, 0.92, -1, StatusOK),
		makeResult("ns", "cpu", "uc", "pod:cpu_throttle_score:5m", "ns", "gvic", "ug", gpuMetric, 0.55, 0, StatusOK),
	}
	got := SelectTopN(results, 10)
	// (gvic, gpu, network) 와 (gvic, gpu, cpu) 두 그룹 각각 rank 1 → 2 항목.
	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (gpu victim 의 network / cpu dimension 각 rank 1)", len(got))
	}
	byDim := map[ResourceDimension]NoisyNeighbor{}
	for _, n := range got {
		if n.VictimSignal != SignalGPU {
			t.Errorf("victim_signal=%s want gpu (suspect=%s)", n.VictimSignal, n.Suspect.Pod)
		}
		if n.Victim.Pod != "gvic" || n.Rank != 1 {
			t.Errorf("victim=%s rank=%d want gvic/1", n.Victim.Pod, n.Rank)
		}
		if n.ImpactOK {
			t.Errorf("gpu victim impactOK=%v want false (impact_seconds 는 latency 전용)", n.ImpactOK)
		}
		byDim[n.Dimension] = n
	}
	if byDim[DimensionNetwork].Score != 0.92 || byDim[DimensionNetwork].Suspect.Pod != "net" {
		t.Errorf("network dimension rank1 불일치: %+v", byDim[DimensionNetwork])
	}
	if byDim[DimensionCPU].Score != 0.55 {
		t.Errorf("cpu dimension score=%g want 0.55", byDim[DimensionCPU].Score)
	}
}

// TestSelectTopN_SuspectScoreNotVictim 은 network_throughput_score / network_retrans_score 가 victim
// 이 아닌 suspect 로 취급되어 (suspect → victim 페어가 성립) 결과가 정상 산출되는지 검증한다.
func TestSelectTopN_SuspectScoreNotVictim(t *testing.T) {
	// suspect=network_throughput_score (victim 아님), victim=latency. 정상 페어 1개.
	results := []CorrelationResult{
		makeResult("ns", "sus", "us", "pod:network_throughput_score:5m", "ns", "vic", "uv", latencyMetric, 0.9, 1, StatusOK),
	}
	got := SelectTopN(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (network_throughput_score 가 victim 으로 오분류되면 0)", len(got))
	}
	if got[0].Dimension != DimensionNetwork || got[0].VictimSignal != SignalLatency {
		t.Errorf("dim=%s signal=%s want network/latency", got[0].Dimension, got[0].VictimSignal)
	}
}

// TestSelectTopN_TwoVictimsNoPair 는 양쪽 모두 victim (latency × throughput) 인 페어가 채택되지 않는지
// 검증한다 (suspect 가 없는 페어).
func TestSelectTopN_TwoVictimsNoPair(t *testing.T) {
	results := []CorrelationResult{
		makeResult("ns", "a", "ua", latencyMetric, "ns", "b", "ub", throughputMetric, 0.9, 1, StatusOK),
	}
	if got := SelectTopN(results, 10); len(got) != 0 {
		t.Errorf("len=%d want 0 (victim×victim 페어는 제외)", len(got))
	}
}

// TestSelectTopN_ImpactGatedToLatency 는 effect size (impact_seconds) 가 latency victim 에만 유효하고
// throughput / error victim 은 ImpactOK=false 로 gate 되는지 검증한다.
func TestSelectTopN_ImpactGatedToLatency(t *testing.T) {
	mk := func(victimMetric string) NoisyNeighbor {
		r := makeResult("ns", "sus", "us", "pod:cpu_throttle_score:5m", "ns", "vic", "uv", victimMetric, 0.9, 1, StatusOK)
		r.Impact = 0.05
		r.ImpactOK = true
		out := SelectTopN([]CorrelationResult{r}, 10)
		if len(out) != 1 {
			t.Fatalf("victim=%s len=%d want 1", victimMetric, len(out))
		}
		return out[0]
	}
	if n := mk(latencyMetric); !n.ImpactOK || n.Impact != 0.05 {
		t.Errorf("latency victim impactOK=%v impact=%g want true/0.05", n.ImpactOK, n.Impact)
	}
	if n := mk(throughputMetric); n.ImpactOK {
		t.Errorf("throughput victim impactOK=%v want false (단위가 seconds 아님)", n.ImpactOK)
	}
	if n := mk(errorMetric); n.ImpactOK {
		t.Errorf("error victim impactOK=%v want false", n.ImpactOK)
	}
	if n := mk(gpuMetric); n.ImpactOK {
		t.Errorf("gpu victim impactOK=%v want false (단위가 seconds 아님)", n.ImpactOK)
	}
}
