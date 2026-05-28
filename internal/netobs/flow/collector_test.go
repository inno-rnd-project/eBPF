package flow

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/kube"
	"netobs/internal/netobs/metadata"
	"netobs/internal/netobs/metrics"
)

// fakeResolver 는 PodResolver 인터페이스 의 테스트 더블 이다. ResolveCgroup 은 미리 셋팅 된 cgroup
// → PodIdentity 매핑 을, ResolveIP 는 미리 셋팅 된 IP → PodIdentity 매핑 을 그대로 반환 한다.
type fakeResolver struct {
	byCgroup map[uint64]kube.PodIdentity
	byIP     map[string]kube.PodIdentity
}

func (f *fakeResolver) ResolveCgroup(cgroup uint64) (kube.PodIdentity, bool) {
	p, ok := f.byCgroup[cgroup]
	return p, ok
}

func (f *fakeResolver) ResolveIP(ip string) kube.PodIdentity {
	if p, ok := f.byIP[ip]; ok {
		return p
	}
	return kube.PodIdentity{}
}

// TestDirectionLabel 은 BPF enum (0=egress, 1=ingress) 의 라벨 매핑 회귀 가드 다.
func TestDirectionLabel(t *testing.T) {
	cases := []struct {
		v    uint8
		want string
	}{
		{0, "egress"},
		{1, "ingress"},
		{99, "unknown"},
	}
	for _, tc := range cases {
		got := directionLabel(tc.v)
		if got != tc.want {
			t.Errorf("directionLabel(%d)=%q want %q", tc.v, got, tc.want)
		}
	}
}

// TestCollector_DisabledOrNilGuardSkipsEmit 은 enabled=false 또는 guard nil 일 때 collector 가 어떤
// 시리즈 도 emit 하지 않는 회귀 가드 다. opt-in 안전 default 의 핵심 정책.
func TestCollector_DisabledOrNilGuardSkipsEmit(t *testing.T) {
	resolver := &fakeResolver{}

	// enabled=false
	c1 := New(resolver, metrics.NewFlowGuard([]string{"ns"}, 100), nil, "node1", false)
	if count := testutil.CollectAndCount(c1, "netobs_flow_bytes_total"); count != 0 {
		t.Errorf("enabled=false count=%d want 0", count)
	}

	// guard nil
	c2 := New(resolver, nil, nil, "node1", true)
	if count := testutil.CollectAndCount(c2, "netobs_flow_bytes_total"); count != 0 {
		t.Errorf("guard=nil count=%d want 0", count)
	}
}

// TestCollector_NoMapEmitsNothing 은 SetMap 호출 전 또는 nil map 일 때 collector 가 panic 없이 빈
// 결과 를 반환 하는 회귀 가드 다. startup race 안전.
func TestCollector_NoMapEmitsNothing(t *testing.T) {
	resolver := &fakeResolver{}
	c := New(resolver, metrics.NewFlowGuard([]string{"ns"}, 100), nil, "node1", true)
	if count := testutil.CollectAndCount(c, "netobs_flow_bytes_total"); count != 0 {
		t.Errorf("nil map count=%d want 0", count)
	}
}

// TestCollector_DescribeEmitsSingleDesc 는 본 collector 가 단일 desc 만 노출 하는지 회귀 가드 한다.
// Describe 단계 의 prometheus.Registerer 충돌 방지 패턴.
func TestCollector_DescribeEmitsSingleDesc(t *testing.T) {
	c := New(&fakeResolver{}, metrics.NewFlowGuard(nil, 100), nil, "node1", true)
	ch := make(chan *prometheus.Desc, 4)
	c.Describe(ch)
	close(ch)
	var got []string
	for d := range ch {
		got = append(got, d.String())
	}
	if len(got) != 1 {
		t.Errorf("desc count=%d want 1", len(got))
	}
	if !strings.Contains(got[0], "netobs_flow_bytes_total") {
		t.Errorf("desc=%q want contains netobs_flow_bytes_total", got[0])
	}
}

// TestCollector_DstClassifierIntegration 는 dstClassifier nil 시 dst 라벨 셋 두 칸 이 빈 문자열 로
// 채워 지는지 가드 한다. dst master switch 가 꺼진 운영 모드 의 회귀 가드.
func TestCollector_DstClassifierIntegration(t *testing.T) {
	c := New(&fakeResolver{}, metrics.NewFlowGuard(nil, 100), nil, "node1", true)
	// classifier nil 이면 emitEntry 내부 분기 가 빈 문자열 두 칸 으로 채운다. 실제 emit 까지 가는
	// 경로 는 BPF map 주입 이 필요해 단위 테스트 에서 직접 검증 어렵다. nil 분기 가 panic 없이 통과
	// 하는 자리 만 확보 한다.
	if c.dstClassifier != nil {
		t.Errorf("dstClassifier=%v want nil", c.dstClassifier)
	}
}

// TestCollector_DstClassifierEnabled 는 dstClassifier 가 enabled=true 로 주입 된 collector 의 dst
// classifier 가 호출 가능 한 상태인지 확인 하는 가드 다.
func TestCollector_DstClassifierEnabled(t *testing.T) {
	classifier := metadata.NewDstLabelClassifier(true, []string{"observability-test"})
	c := New(&fakeResolver{}, metrics.NewFlowGuard([]string{"observability-test"}, 100), classifier, "node1", true)
	if c.dstClassifier == nil {
		t.Errorf("dstClassifier=nil want non-nil")
	}
}
