// Package safety 는 workload-injector 가 prod 자동 실행 등 silent misuse 를 차단하는 가드 모음이다.
// 본 이슈 본문의 비목표 (prod 자동 실행 금지) 를 binary 단에서 명시 검증해 cluster label 이 일치
// 하지 않거나 입력이 상한을 초과하면 fail-fast 한다.
package safety

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"netobs/internal/injector/loadgen"
)

// DurationLimit 는 injection 의 절대 상한이다. 30 분을 초과하는 부하는 운영 안전성 측면에서 본
// 도구의 사용 패턴 (dev cluster 한 cycle 검증) 을 벗어나는 misuse 로 본다.
const DurationLimit = 30 * time.Minute

// CheckDuration 은 duration 이 양수이고 DurationLimit 이하인지 검증한다.
func CheckDuration(d time.Duration) error {
	if d <= 0 {
		return errors.New("duration must be positive")
	}
	if d > DurationLimit {
		return fmt.Errorf("duration %s exceeds limit %s", d, DurationLimit)
	}
	return nil
}

// IntensityLimits 는 kind 별 intensity 상한이다. 운영자 입력 실수로 노드를 통째로 점유하는 부하가
// 발생하지 않도록 cgroup limit 차원에서 가드한다.
//
//   - cpu: cpu millis (`4000m`) 이하. stress --cpu N 의 worker thread 가 4 core 이하로 제한.
//   - memory: bytes (`2Gi`) 이하. K8s resource.Quantity 규약 (binary prefix Ki/Mi/Gi, decimal K/M/G)
//     을 그대로 따라 loadgen/memory.go 의 cgroup limit 와 stress --vm-bytes 와 본 가드의 의미를
//     단일 진실원으로 일치시킨다. 노드 가용 메모리의 일부 수준으로 제한해 OOM 충격이 다른 워크로드
//     에 전이되지 않게 한다.
//   - network: bandwidth (`1000M`) 이하. iperf3 -b 의 상한.
//   - gpu: 목표 점유율 percent (`100`) 이하. loadgen/gpu.go 가 본 percent 를 CUDA busy kernel 의
//     duty cycle 로 매핑해 단일 GPU device 의 SM 점유율을 근사한다. device 수가 아니라 점유율이라
//     단일 GPU 노드에서도 부하 강도를 1~100 으로 조절할 수 있다.
var IntensityLimits = map[loadgen.Kind]string{
	loadgen.KindCPU:     "4000m",
	loadgen.KindMemory:  "2Gi",
	loadgen.KindNetwork: "1000M",
	loadgen.KindGPU:     "100",
}

// CheckIntensity 는 kind 별 상한 표 와 입력을 비교한다. cpu 는 resource.Quantity 로 파싱해 수치 비교,
// gpu 는 1~100 점유율 percent 로 파싱, memory 는 bytes 단위로 파싱, network 는 단순 prefix 매칭으로
// 1000M / 1G 이상 거부.
func CheckIntensity(kind loadgen.Kind, intensity string) error {
	limit, ok := IntensityLimits[kind]
	if !ok {
		return fmt.Errorf("unknown kind %q", kind)
	}
	switch kind {
	case loadgen.KindCPU:
		return checkResourceQuantity(intensity, limit, "cpu")
	case loadgen.KindGPU:
		return checkGPUUtilPercent(intensity, limit)
	case loadgen.KindMemory:
		return checkMemoryBytes(intensity, limit)
	case loadgen.KindNetwork:
		return checkBandwidth(intensity, limit)
	}
	return nil
}

// checkGPUUtilPercent 는 gpu intensity 를 1~limit 범위의 정수 점유율 percent 로 검증한다. loadgen/gpu.go
// 가 동일 규약으로 percent 를 CUDA duty cycle 로 매핑하므로 0 이하나 비정수, 상한 초과를 fail-fast 한다.
func checkGPUUtilPercent(input, limit string) error {
	in, err := atoi64(strings.TrimSpace(input))
	if err != nil {
		return fmt.Errorf("parse gpu intensity %q as utilization percent: %w", input, err)
	}
	max, err := atoi64(strings.TrimSpace(limit))
	if err != nil {
		return fmt.Errorf("parse gpu limit %q: %w", limit, err)
	}
	if in < 1 {
		return fmt.Errorf("gpu intensity %s must be a positive utilization percent", input)
	}
	if in > max {
		return fmt.Errorf("gpu intensity %s exceeds limit %s%%", input, limit)
	}
	return nil
}

// CheckClusterLabel 은 cluster Node 중 requiredLabel (예: `environment=dev`) 와 일치하는 Node 가
// 1 개 이상 있는지 검증한다. 본 가드의 효과는 prod cluster 의 Node 가 `environment=prod` 만 가지
// 도록 라벨링되어 있어야 의미가 있다. 운영자가 라벨링 규약을 지킨다는 가정 하에 동작한다.
//
// requiredLabel 이 빈 문자열이면 검사를 skip 한다 (운영자가 의도적으로 비활성화).
func CheckClusterLabel(ctx context.Context, client kubernetes.Interface, requiredLabel string) error {
	requiredLabel = strings.TrimSpace(requiredLabel)
	if requiredLabel == "" {
		return nil
	}
	parts := strings.SplitN(requiredLabel, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid label format %q (expected key=value)", requiredLabel)
	}
	key, value := parts[0], parts[1]
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", key, value),
	})
	if err != nil {
		return fmt.Errorf("list nodes by label: %w", err)
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("no node matches required label %s (refusing to run; verify INJECTOR_ALLOW_CLUSTER_LABEL)", requiredLabel)
	}
	return nil
}

// LockName 은 ConfigMap annotation lease 의 이름 규칙이다. target Pod 별 별도 lock 으로 동일 target
// 에 동시 injection 만 차단하고 다른 target 은 병렬 허용한다.
func LockName(targetNamespace, targetPod string) string {
	return fmt.Sprintf("workload-injector-lock-%s-%s", targetNamespace, targetPod)
}

// AcquireLock 은 ConfigMap annotation 기반 lease 로 동일 target 동시 injection 을 차단한다. 같은
// target 에 이미 lease 가 있고 TTL 이 만료되지 않았으면 fail-fast. release 함수는 lease 가 정상
// 종료될 때 호출되어 다음 injection 이 즉시 진행되도록 한다.
func AcquireLock(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, targetNamespace, targetPod string,
	holder string,
	ttl time.Duration,
) (release func(), err error) {
	name := LockName(targetNamespace, targetPod)
	now := time.Now().UTC()
	expire := now.Add(ttl)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				"injector.lease/holder":  holder,
				"injector.lease/expires": expire.Format(time.RFC3339),
			},
			Labels: map[string]string{
				"app.kubernetes.io/name":      "workload-injector",
				"app.kubernetes.io/component": "lease",
			},
		},
	}
	_, err = client.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err == nil {
		return func() { _ = client.CoreV1().ConfigMaps(namespace).Delete(ctx, name, metav1.DeleteOptions{}) }, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	// 이미 존재하는 lease 의 TTL 검사.
	existing, getErr := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		return nil, fmt.Errorf("get existing lease: %w", getErr)
	}
	expiresStr := existing.Annotations["injector.lease/expires"]
	if expiresStr == "" {
		return nil, fmt.Errorf("lease %s exists without expires annotation (manual cleanup required)", name)
	}
	expires, perr := time.Parse(time.RFC3339, expiresStr)
	if perr != nil {
		return nil, fmt.Errorf("parse lease expires %q: %w", expiresStr, perr)
	}
	if now.Before(expires) {
		held := existing.Annotations["injector.lease/holder"]
		return nil, fmt.Errorf("lease held by %q until %s (refusing concurrent injection)", held, expiresStr)
	}
	// 만료된 lease 는 강제 갱신.
	existing.Annotations["injector.lease/holder"] = holder
	existing.Annotations["injector.lease/expires"] = expire.Format(time.RFC3339)
	if _, err := client.CoreV1().ConfigMaps(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("steal expired lease: %w", err)
	}
	return func() { _ = client.CoreV1().ConfigMaps(namespace).Delete(ctx, name, metav1.DeleteOptions{}) }, nil
}

func checkResourceQuantity(input, limit, kind string) error {
	in, err := parseQuantity(input)
	if err != nil {
		return fmt.Errorf("parse %s intensity %q: %w", kind, input, err)
	}
	max, err := parseQuantity(limit)
	if err != nil {
		return fmt.Errorf("parse %s limit %q: %w", kind, limit, err)
	}
	if in.cmp(max) > 0 {
		return fmt.Errorf("%s intensity %s exceeds limit %s", kind, input, limit)
	}
	return nil
}

// 단위 변환 helper. resource.Quantity 의존을 피해 패키지 외부 의존을 최소화한다. cpu 는 "500m" /
// "4" 형식, gpu 는 "1" / "2" 형식 모두 정수 millis 로 정규화.
type quantity struct {
	millis int64
}

func (a quantity) cmp(b quantity) int {
	switch {
	case a.millis < b.millis:
		return -1
	case a.millis > b.millis:
		return 1
	}
	return 0
}

func parseQuantity(s string) (quantity, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return quantity{}, errors.New("empty quantity")
	}
	if strings.HasSuffix(s, "m") {
		v, err := atoi64(strings.TrimSuffix(s, "m"))
		if err != nil {
			return quantity{}, err
		}
		return quantity{millis: v}, nil
	}
	v, err := atoi64(s)
	if err != nil {
		return quantity{}, err
	}
	return quantity{millis: v * 1000}, nil
}

// checkMemoryBytes 는 stress --vm-bytes 입력을 K8s resource.Quantity 규약 (K/M/G decimal,
// Ki/Mi/Gi binary) 으로 파싱해 limit 와 비교한다. loadgen/memory.go 가 동일 규약으로 cgroup limit
// 와 stress 인자를 만들어 단위 해석 단일 진실원이 유지된다.
func checkMemoryBytes(input, limit string) error {
	in, err := parseMemoryBytes(input)
	if err != nil {
		return fmt.Errorf("parse memory intensity %q: %w", input, err)
	}
	max, err := parseMemoryBytes(limit)
	if err != nil {
		return fmt.Errorf("parse memory limit %q: %w", limit, err)
	}
	if in > max {
		return fmt.Errorf("memory intensity %s exceeds limit %s", input, limit)
	}
	return nil
}

// parseMemoryBytes 는 K8s resource.Quantity 로 입력을 파싱해 bytes 정수로 변환한다. 단위 규약은
// Kubernetes 표준을 그대로 따라 K=10^3 / M=10^6 / G=10^9 (decimal) 와 Ki=2^10 / Mi=2^20 / Gi=2^30
// (binary) 가 의미가 다르게 해석된다. 운영자가 표준 규약에 맞춰 입력하면 cgroup limit 와 stress
// 가드가 동일 의미를 갖는다.
func parseMemoryBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty memory")
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, err
	}
	return q.Value(), nil
}

func checkBandwidth(input, limit string) error {
	in, err := parseBandwidthBps(input)
	if err != nil {
		return fmt.Errorf("parse network intensity %q: %w", input, err)
	}
	max, err := parseBandwidthBps(limit)
	if err != nil {
		return fmt.Errorf("parse network limit %q: %w", limit, err)
	}
	if in > max {
		return fmt.Errorf("network intensity %s exceeds limit %s", input, limit)
	}
	return nil
}

// parseBandwidthBps 는 iperf3 -b 의 입력 (예: "100M", "1G", "500K") 을 bits/sec 정수로 변환한다.
func parseBandwidthBps(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty bandwidth")
	}
	mult := int64(1)
	switch last := s[len(s)-1]; last {
	case 'K', 'k':
		mult = 1_000
		s = s[:len(s)-1]
	case 'M', 'm':
		mult = 1_000_000
		s = s[:len(s)-1]
	case 'G', 'g':
		mult = 1_000_000_000
		s = s[:len(s)-1]
	}
	v, err := atoi64(s)
	if err != nil {
		return 0, err
	}
	return v * mult, nil
}

func atoi64(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty number")
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid digit %q", c)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
