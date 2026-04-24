package types

import (
	"encoding/binary"
	"testing"
)

// -------------------------------------------------------------------
// U32ToIPv4
// -------------------------------------------------------------------

func TestU32ToIPv4(t *testing.T) {
	// NativeEndian 기반 변환: BPF가 기록한 네트워크 바이트 순서 IP를
	// NativeEndian uint32로 읽으면 Go 쪽에서도 동일한 IP 문자열이 나와야 한다.
	cases := []struct {
		octets [4]byte
		want   string
	}{
		{[4]byte{192, 168, 1, 1}, "192.168.1.1"},
		{[4]byte{10, 0, 0, 1}, "10.0.0.1"},
		{[4]byte{8, 8, 8, 8}, "8.8.8.8"},
		{[4]byte{0, 0, 0, 0}, "0.0.0.0"},
		{[4]byte{255, 255, 255, 255}, "255.255.255.255"},
	}

	for _, c := range cases {
		v := binary.NativeEndian.Uint32(c.octets[:])
		got := U32ToIPv4(v)
		if got != c.want {
			t.Errorf("U32ToIPv4(%v) = %q, want %q", c.octets, got, c.want)
		}
	}
}

// -------------------------------------------------------------------
// TrimGeneratedSuffix
// -------------------------------------------------------------------

func TestTrimGeneratedSuffix(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// Deployment 해시 suffix (10자 소문자 영숫자) 제거
		{"nginx-deployment-7bd9b4b5b7", "nginx-deployment"},
		// ReplicaSet 해시 suffix (9자) 제거
		{"my-app-abcdef123", "my-app"},
		// 12자 해시 suffix 제거
		{"web-backend-abcdef123456", "web-backend"},
		// suffix 없음 (단일 세그먼트)
		{"myapp", ""},
		// suffix가 너무 짧음 (< 8자)
		{"my-app-abc", ""},
		// suffix가 너무 김 (> 16자, 17자)
		{"my-app-abcdefghijklmnopq", ""},
		// suffix에 대문자 포함 (hash-like가 아님)
		{"my-app-ABCDEFGH", ""},
		// suffix에 특수문자 포함
		{"my-app-abc-defg", ""},
		// 다중 세그먼트, 마지막이 해시
		{"prefix-middle-abcdef1234", "prefix-middle"},
	}

	for _, c := range cases {
		got := TrimGeneratedSuffix(c.input)
		if got != c.want {
			t.Errorf("TrimGeneratedSuffix(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// -------------------------------------------------------------------
// PodIdentity.Rank
// -------------------------------------------------------------------

func TestPodIdentityRank(t *testing.T) {
	cases := []struct {
		class string
		want  int
	}{
		{IdentityClassPod, 5},
		{IdentityClassService, 4},
		{IdentityClassNode, 3},
		{IdentityClassExternal, 2},
		{IdentityClassUnresolved, 1},
		{"", 0},
		{"unknown-class", 0},
	}

	for _, c := range cases {
		p := PodIdentity{IdentityClass: c.class}
		if got := p.Rank(); got != c.want {
			t.Errorf("Rank(IdentityClass=%q) = %d, want %d", c.class, got, c.want)
		}
	}
}

// -------------------------------------------------------------------
// PodIdentity.WorkloadKey
// -------------------------------------------------------------------

func TestPodIdentityWorkloadKey(t *testing.T) {
	cases := []struct {
		name string
		id   PodIdentity
		want string
	}{
		{
			name: "pod with deployment owner",
			id: PodIdentity{
				IdentityClass: IdentityClassPod,
				Namespace:     "my-ns",
				WorkloadKind:  "Deployment",
				Workload:      "my-deploy",
				PodName:       "my-deploy-abc1234567-xxx",
			},
			want: "my-ns/Deployment/my-deploy",
		},
		{
			name: "pod without owner (bare pod)",
			id: PodIdentity{
				IdentityClass: IdentityClassPod,
				Namespace:     "default",
				WorkloadKind:  "Pod",
				Workload:      "standalone-pod",
				PodName:       "standalone-pod",
			},
			want: "default/Pod/standalone-pod",
		},
		{
			name: "service",
			id: PodIdentity{
				IdentityClass: IdentityClassService,
				Namespace:     "prod",
				WorkloadKind:  "Service",
				Workload:      "frontend",
			},
			want: "prod/Service/svc/frontend",
		},
		{
			name: "node",
			id: PodIdentity{
				IdentityClass: IdentityClassNode,
				NodeName:      "worker-1",
				WorkloadKind:  "Node",
				Workload:      "worker-1",
			},
			want: "host/Node/node/worker-1",
		},
		{
			name: "external",
			id: PodIdentity{
				IdentityClass: IdentityClassExternal,
				PodIP:         "8.8.8.8",
			},
			want: "external/External/external",
		},
		{
			name: "unresolved",
			id: PodIdentity{
				IdentityClass: IdentityClassUnresolved,
			},
			want: "unresolved/Unresolved/unresolved",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.id.WorkloadKey()
			if got != c.want {
				t.Errorf("WorkloadKey() = %q, want %q", got, c.want)
			}
		})
	}
}

// -------------------------------------------------------------------
// StageName
// -------------------------------------------------------------------

func TestStageName(t *testing.T) {
	cases := []struct {
		stage uint8
		want  string
	}{
		{StageSendmsgRet, "sendmsg_ret"},
		{StageToVeth, "to_veth"},
		{StageToDevQ, "to_devq"},
		{StageRetrans, "retrans"},
		{StageDrop, "drop"},
		{0, "unknown"},
		{99, "unknown"},
	}

	for _, c := range cases {
		got := StageName(c.stage)
		if got != c.want {
			t.Errorf("StageName(%d) = %q, want %q", c.stage, got, c.want)
		}
	}
}
