//go:build !nccl

package nccl

import "testing"

// TestNewProduction_StubIsNoop은 기본 빌드 (build tag nccl 비활성) 에서 NewProduction이 noop을
// 돌려주는지 검증한다. dev cluster의 RTX 3090 환경에서 production attach 없이 graceful
// degradation으로 Available=false와 closed Events 채널이 유지되는 회귀 가드다.
func TestNewProduction_StubIsNoop(t *testing.T) {
	p := NewProduction("/host/usr/lib/x86_64-linux-gnu/libnccl.so.2", "test-node")
	defer func() { _ = p.Close() }()

	if p.Available() {
		t.Errorf("stub NewProduction.Available()=true want false")
	}
	if err := p.Attach(); err != nil {
		t.Errorf("stub NewProduction.Attach()=%v want nil", err)
	}
	count := 0
	for range p.Events() {
		count++
	}
	if count != 0 {
		t.Errorf("stub NewProduction.Events() emitted %d want 0 (closed channel)", count)
	}
}
