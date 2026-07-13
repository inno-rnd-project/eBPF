package nvml

import (
	"testing"

	gonvml "github.com/NVIDIA/go-nvml/pkg/nvml"
)

// TestMergeRunningProcesses 는 compute / graphics 병합의 PID dedupe (max memory 채택) 와 실행 모드
// 타입 판정, PID 오름차순 정렬을 검증한다.
func TestMergeRunningProcesses(t *testing.T) {
	compute := []gonvml.ProcessInfo{
		{Pid: 300, UsedGpuMemory: 10},
		{Pid: 100, UsedGpuMemory: 5},
	}
	graphics := []gonvml.ProcessInfo{
		{Pid: 100, UsedGpuMemory: 8}, // compute 와 중복 → max 8, compute+graphics
		{Pid: 200, UsedGpuMemory: 3},
	}
	got := mergeRunningProcesses(compute, graphics, 2)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3: %+v", len(got), got)
	}
	// PID 오름차순: 100, 200, 300.
	if got[0].PID != 100 || got[1].PID != 200 || got[2].PID != 300 {
		t.Fatalf("정렬 어긋남: %+v", got)
	}
	if got[0].MemoryUsedBytes != 8 || got[0].Type != "compute+graphics" {
		t.Errorf("pid 100=%+v want mem 8 (max)/compute+graphics", got[0])
	}
	if got[1].MemoryUsedBytes != 3 || got[1].Type != "graphics" {
		t.Errorf("pid 200=%+v want mem 3/graphics", got[1])
	}
	if got[2].MemoryUsedBytes != 10 || got[2].Type != "compute" {
		t.Errorf("pid 300=%+v want mem 10/compute", got[2])
	}
	if got[0].DeviceIndex != 2 {
		t.Errorf("device_index=%d want 2", got[0].DeviceIndex)
	}
}

// TestMergeRunningProcesses_Empty 는 양쪽 목록이 비면 빈 슬라이스를 돌려주는지 검증한다.
func TestMergeRunningProcesses_Empty(t *testing.T) {
	if got := mergeRunningProcesses(nil, nil, 0); len(got) != 0 {
		t.Errorf("got=%+v want empty", got)
	}
}
