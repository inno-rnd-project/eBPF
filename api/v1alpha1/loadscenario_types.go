package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConcurrencyPolicy 는 LoadScenario 가 동일 target 의 진행 중 injection 과 겹칠 때의 처리 정책이다.
// CronJob convention 을 차용해 운영자가 익숙한 의미 그대로 사용 가능 하게 한다.
// +kubebuilder:validation:Enum=Allow;Forbid;Replace
type ConcurrencyPolicy string

const (
	// ConcurrencyAllow 는 진행 중 injection 이 있어도 동일 target 이 아니면 새 run 을 트리거 한다.
	// 단일 target 의 동시 Allow 는 AcquireLock 자체 가 차단 한다.
	ConcurrencyAllow ConcurrencyPolicy = "Allow"
	// ConcurrencyForbid 는 진행 중 injection 이 끝날 때까지 다음 run 을 skip 한다.
	ConcurrencyForbid ConcurrencyPolicy = "Forbid"
	// ConcurrencyReplace 는 진행 중 injection 의 lock 을 해제 한 뒤 새 run 을 트리거 한다.
	ConcurrencyReplace ConcurrencyPolicy = "Replace"
)

// LoadKind 는 부하 종류 enum 이다. internal/injector/loadgen.Kind 와 1:1 매핑 되며 string 값도
// 동일 ("cpu" / "memory" / "network" / "gpu") 하여 controller 에서 그대로 변환 사용 한다.
// +kubebuilder:validation:Enum=cpu;memory;network;gpu
type LoadKind string

const (
	LoadKindCPU     LoadKind = "cpu"
	LoadKindMemory  LoadKind = "memory"
	LoadKindNetwork LoadKind = "network"
	LoadKindGPU     LoadKind = "gpu"
)

// LoadScenarioTargetRef 는 부하 대상 Pod 식별자다. 기존 CLI 의 -target-namespace / -target-pod
// 플래그 와 동일 의미 다.
type LoadScenarioTargetRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// LoadScenarioSpec 는 LoadScenario 의 사용자 정의 입력이다.
type LoadScenarioSpec struct {
	// Schedule 은 cron 표현식 또는 "@every <duration>" 형식이다. controller 의 cron parser 가
	// 본 문자열을 next run time 산정에 사용 한다. 예: "0 0 * * *" (매일 자정), "@every 5m".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Schedule string `json:"schedule"`

	// Kind 는 부하 종류 enum 이다.
	// +kubebuilder:validation:Required
	Kind LoadKind `json:"kind"`

	// Duration 은 1 회 부하 인가 시간이다. internal/injector/safety.CheckDuration 의 상한
	// (30 분) 을 따른다. metav1.Duration 으로 표기 해 "5m" 같은 친숙한 표현 을 받는다.
	// +kubebuilder:validation:Required
	Duration metav1.Duration `json:"duration"`

	// Intensity 는 부하 강도다. kind 별 단위 가 다르며 (cpu: millicores, memory: Mi/Gi,
	// network: M/G, gpu: 1 단위 fraction) safety.CheckIntensity 에서 kind 별 상한과 비교한다.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Intensity string `json:"intensity"`

	// TargetRef 는 부하 대상 Pod 다.
	// +kubebuilder:validation:Required
	TargetRef LoadScenarioTargetRef `json:"targetRef"`

	// ConcurrencyPolicy 는 진행 중 injection 과 겹칠 때 의 처리 정책 이다. 기본값 Forbid.
	// +kubebuilder:default=Forbid
	// +optional
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// SpikeAlertAssertion 이 true 이면 controller 가 부하 종료 후 5 분 polling window 동안
	// z-score spike alert (CPUThrottleSpike / MemoryPressureSpike / NetworkDropSpike /
	// GPUUtilizationSpike) 발화 여부 를 Prometheus query 로 확인 해 status 에 기록 한다.
	// +kubebuilder:default=false
	// +optional
	SpikeAlertAssertion bool `json:"spikeAlertAssertion,omitempty"`

	// MaxFailures 는 연속 실패 임계 이다. status.consecutiveFailures 가 본 값을 초과 하면
	// LoadScenario 의 suspend 상태 가 true 로 자동 전환 되어 다음 run 이 트리거 되지 않는다.
	// 기본값 3.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxFailures int32 `json:"maxFailures,omitempty"`

	// Suspend 가 true 이면 schedule 따른 다음 run 이 트리거 되지 않는다. 운영자가 임시 disable
	// 하거나 controller 의 maxFailures 가드 가 자동 설정 한다.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// LoadScenarioStatus 는 controller 가 reconcile 결과 를 기록 하는 영역이다.
type LoadScenarioStatus struct {
	// LastScheduleTime 은 controller 가 마지막 으로 run 을 트리거 한 시각 이다.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulRunTime 은 마지막 으로 정상 종료 된 run 의 시각 이다.
	// +optional
	LastSuccessfulRunTime *metav1.Time `json:"lastSuccessfulRunTime,omitempty"`

	// LastObservedSpikeAlerts 는 spikeAlertAssertion 이 true 일 때 마지막 run 종료 직후
	// firing 으로 관측 된 alertname 목록 이다. hit 가 없으면 빈 배열 또는 nil.
	// +optional
	LastObservedSpikeAlerts []string `json:"lastObservedSpikeAlerts,omitempty"`

	// ConsecutiveFailures 는 마지막 successful run 이후 연속 실패 횟수 이다. successful run 마다 0
	// 으로 reset 된다.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// Conditions 는 controller-runtime convention 의 condition 배열 이다. 본 CRD 가 노출 하는
	// condition type 은 다음 4 종 이다.
	//   - Ready (controller 가 LoadScenario 를 reconcile 가능 한 상태)
	//   - Scheduled (마지막 run 이 schedule 따라 정상 트리거 됨)
	//   - SpikeAlertObserved (spikeAlertAssertion = true 일 때 마지막 run 의 alert hit 여부)
	//   - Suspended (maxFailures 초과 또는 운영자 명시 suspend)
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// LoadScenario 는 dev cluster 에서 schedule 따라 자동 부하 를 인가 하는 CRD 다.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ls
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:JSONPath=.spec.schedule,name=Schedule,type=string
// +kubebuilder:printcolumn:JSONPath=.spec.kind,name=Kind,type=string
// +kubebuilder:printcolumn:JSONPath=.status.lastScheduleTime,name=LastSchedule,type=date
// +kubebuilder:printcolumn:JSONPath=.status.lastSuccessfulRunTime,name=LastSuccess,type=date
// +kubebuilder:printcolumn:JSONPath=.spec.suspend,name=Suspended,type=boolean
type LoadScenario struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LoadScenarioSpec   `json:"spec,omitempty"`
	Status LoadScenarioStatus `json:"status,omitempty"`
}

// LoadScenarioList 는 List 동작 의 응답 타입 이다.
// +kubebuilder:object:root=true
type LoadScenarioList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LoadScenario `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LoadScenario{}, &LoadScenarioList{})
}
