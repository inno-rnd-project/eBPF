package cuda

import "testing"

func TestDeviceMap_LookupReturnsUUID(t *testing.T) {
	d := newDeviceMap()
	d.replace(map[uint32]string{
		1234: "GPU-A",
		5678: "GPU-B",
	})

	if got := d.lookup(1234); got != "GPU-A" {
		t.Errorf("lookup(1234)=%q want GPU-A", got)
	}
	if got := d.lookup(5678); got != "GPU-B" {
		t.Errorf("lookup(5678)=%q want GPU-B", got)
	}
}

func TestDeviceMap_LookupMissReturnsEmpty(t *testing.T) {
	d := newDeviceMap()
	d.replace(map[uint32]string{1: "GPU-A"})

	if got := d.lookup(99); got != "" {
		t.Errorf("lookup(99)=%q want empty (miss)", got)
	}
}

func TestDeviceMap_ReplaceSwapsEntireSnapshot(t *testing.T) {
	// replace 는 add/merge 가 아닌 atomic 통째 교체. 첫 snapshot 의 PID 가 두 번째 snapshot 에서 빠지면
	// lookup miss 가 되어야 한다 — 종료된 프로세스의 stale 매핑이 남지 않게 한다.
	d := newDeviceMap()
	d.replace(map[uint32]string{1: "GPU-A", 2: "GPU-B"})
	d.replace(map[uint32]string{2: "GPU-B"})

	if got := d.lookup(1); got != "" {
		t.Errorf("after replace, removed pid lookup=%q want empty", got)
	}
	if got := d.lookup(2); got != "GPU-B" {
		t.Errorf("survived pid lookup=%q want GPU-B", got)
	}
}

func TestDeviceMap_EmptyOnInit(t *testing.T) {
	d := newDeviceMap()
	if got := d.lookup(1); got != "" {
		t.Errorf("fresh map lookup=%q want empty", got)
	}
}
