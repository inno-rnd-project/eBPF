// Package loadgen 은 workload-injector 가 cpu / memory / network / gpu 4 종류의 부하를 발생시키는
// 모듈들의 공통 인터페이스다. 각 모듈은 K8s API 로 target node 에 stress Pod 를 spawn 해 cgroup
// 격리 안에서 부하를 발생시키고 종료 시 spawn 한 Pod 를 idempotent 하게 삭제한다.
package loadgen

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Kind 는 부하 종류 enum 이다. exporter 가 메트릭 라벨로 본 값을 그대로 사용한다.
type Kind string

const (
	KindCPU     Kind = "cpu"
	KindMemory  Kind = "memory"
	KindNetwork Kind = "network"
	KindGPU     Kind = "gpu"
)

// Params 는 LoadGenerator.Start 가 받는 입력이다. injector main 이 운영자 환경 변수 / flag 로부터
// 채워서 전달한다.
type Params struct {
	TargetNode      string
	TargetNamespace string
	TargetPod       string
	// SpawnNamespace 는 stress Pod 가 생성되는 네임스페이스다. RBAC 가드 측면에서 target 네임스페이스
	// 와 분리 가능하도록 별도 필드로 둔다.
	SpawnNamespace string
	Duration       time.Duration
	Intensity      string
	// Labels 는 spawn 되는 Pod 에 공통으로 부여될 라벨이다. PodMonitor selector 또는 cleanup 식별에
	// 활용된다.
	Labels map[string]string
	// Owner 는 spawn 되는 Pod 의 ownerReference 다 (#418). 컨트롤러 pod 를 소유자로 두면 컨트롤러
	// (Deployment) 삭제 시 Kubernetes GC 가 부하 pod 를 회수한다. ownerReference 는 동일 namespace
	// 제약이 있어 (cross-namespace owner 는 GC 가 소유자 부재로 간주해 즉시 회수) 호출자가
	// SpawnNamespace 와 자신의 namespace 가 일치할 때만 채운다. nil 이면 미부여.
	Owner *metav1.OwnerReference
}

// LoadGenerator 는 세 부하 모듈이 공통으로 구현하는 인터페이스다. Start 가 비차단 (Pod 만 띄우고
// 즉시 return) 이며 injector main 이 Duration 만큼 sleep 후 Stop 을 호출한다. Stop 은 idempotent 라
// 같은 인스턴스에 두 번 호출되어도 NotFound 를 무시한다.
type LoadGenerator interface {
	Kind() Kind
	Start(ctx context.Context, params Params) error
	Stop(ctx context.Context) error
	SpawnedPods() []types.NamespacedName
}

// New 는 kind 에 맞는 LoadGenerator 구현체를 반환한다. client 가 nil 이면 fail-fast.
func New(kind Kind, client kubernetes.Interface) (LoadGenerator, error) {
	if client == nil {
		return nil, errors.New("loadgen: kubernetes client is nil")
	}
	switch kind {
	case KindCPU:
		return &cpuGen{client: client}, nil
	case KindMemory:
		return &memoryGen{client: client}, nil
	case KindNetwork:
		return &networkGen{client: client}, nil
	case KindGPU:
		return &gpuGen{client: client}, nil
	}
	return nil, fmt.Errorf("loadgen: unknown kind %q", kind)
}

// commonPodSpec 는 세 모듈이 공유하는 Pod 설정이다. nodeName 으로 노드 강제 배치, hostPID / hostNetwork
// 미사용, runAsNonRoot 는 모듈별 stress 도구 호환성 때문에 별도 결정.
func commonPodMeta(name string, params Params) metav1.ObjectMeta {
	labels := map[string]string{
		"app.kubernetes.io/name":      "workload-injector",
		"app.kubernetes.io/component": "stress",
		"app.kubernetes.io/part-of":   "ebpf-project",
		// 호출자가 모듈별 (cpu / network / gpu) 값으로 명시 덮어쓰는 라벨이다. 기본값 unknown 으로
		// 두어 덮어쓰기 누락 시 silent bug 가 아닌 라벨 값으로 즉시 드러나게 한다.
		"injector.kind": "unknown",
	}
	for k, v := range params.Labels {
		labels[k] = v
	}
	meta := metav1.ObjectMeta{
		Name:      name,
		Namespace: params.SpawnNamespace,
		Labels:    labels,
	}
	if params.Owner != nil {
		meta.OwnerReferences = []metav1.OwnerReference{*params.Owner}
	}
	return meta
}

// SelfOwnerReference 는 downward API env (POD_NAME / POD_NAMESPACE / POD_UID) 로부터 자기 pod 의
// ownerReference 를 만든다 (#418). spawnNamespace 가 자기 namespace 와 다르거나 env 가 비어 있으면
// (수동 실행 등) nil 을 돌려줘 미부여로 진행한다. controller=false 로 두어 kubelet / scheduler 의
// 소유권 판정에 관여하지 않고 GC 회수 용도로만 쓴다.
func SelfOwnerReference(podName, podNamespace, podUID, spawnNamespace string) *metav1.OwnerReference {
	if podName == "" || podUID == "" || podNamespace == "" || podNamespace != spawnNamespace {
		return nil
	}
	return &metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Pod",
		Name:       podName,
		UID:        types.UID(podUID),
	}
}

// deadlineGraceSec 는 activeDeadlineSeconds 의 여유분이다 (#418). 부하 duration 에 이미지 pull 과
// 스케줄 대기와 client 의 server ready polling (최대 수십 초) 을 더해도 남는 상한으로, kubelet 이
// 컨트롤러 부재 시에도 pod 를 강제 종료하는 최후 방어선이다. 정상 경로 (자체 종료 또는 Stop 의
// delete) 보다 항상 늦게 걸리도록 duration 대비 충분히 크게 둔다.
const deadlineGraceSec = 120

// activeDeadlineSeconds 는 부하 duration + 여유의 kubelet 강제 종료 상한을 돌려준다 (#418).
// 전 부하 pod 의 PodSpec.ActiveDeadlineSeconds 에 공통 적용된다.
func activeDeadlineSeconds(params Params) *int64 {
	d := int64(params.Duration.Seconds()) + deadlineGraceSec
	return &d
}

// createPod 는 K8s API 로 Pod 생성을 시도하고 이미 존재하면 NotFound / AlreadyExists 를 무시한다.
// 부하 모듈은 Pod 생성 실패를 startError 로 분류해 cleanup 흐름으로 전이한다.
func createPod(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod) error {
	_, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("create pod %s/%s: %w", pod.Namespace, pod.Name, err)
}

// deletePod 는 spawn 한 Pod 를 삭제한다. NotFound 는 정상 (cleanup 흐름의 idempotent 보장).
func deletePod(ctx context.Context, client kubernetes.Interface, name types.NamespacedName) error {
	err := client.CoreV1().Pods(name.Namespace).Delete(ctx, name.Name, metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("delete pod %s: %w", name, err)
}

// dns1123Sanitizer 는 DNS-1123 라벨 규칙 위반 문자를 하이픈으로 치환하기 위한 정규식이다.
// K8s Pod 이름은 lowercase 영문 / 숫자 / 하이픈만 허용하므로 target Pod 이름에 대문자나 특수문자가
// 있어도 안전하게 stress Pod 이름을 만들 수 있게 한다.
var dns1123Sanitizer = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeName 은 prefix-target 형태의 Pod 이름을 DNS-1123 규칙에 맞추어 정규화한다. 대문자는
// 소문자로, 허용되지 않는 문자는 하이픈으로 치환하고 63 자 상한을 적용한다. 본 helper 는 cpu /
// network / gpu 세 모듈이 spawn 하는 Pod 이름의 일관된 정규화 단일 지점이다.
func sanitizeName(prefix, target string) string {
	target = strings.ToLower(target)
	target = dns1123Sanitizer.ReplaceAllString(target, "-")
	target = strings.Trim(target, "-")
	name := fmt.Sprintf("%s-%s", prefix, target)
	if len(name) > 63 {
		name = name[:63]
	}
	name = strings.Trim(name, "-")
	if name == "" {
		name = prefix
	}
	return name
}

// deleteAll 은 다중 spawn 된 Pod 를 한 번에 cleanup 한다. 일부 삭제 실패가 있어도 나머지를 계속
// 시도하고 마지막에 wrapped error 를 반환한다.
func deleteAll(ctx context.Context, client kubernetes.Interface, names []types.NamespacedName) error {
	var lastErr error
	for _, n := range names {
		if err := deletePod(ctx, client, n); err != nil {
			lastErr = err
		}
	}
	return lastErr
}
