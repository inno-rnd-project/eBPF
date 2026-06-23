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
	// z-score spike alert (CPUThrottleSpikeDetected / MemoryPressureSpikeDetected /
	// NetworkDropSpikeDetected / GPUUtilSpikeDetected) 발화 여부 를 Prometheus query 로 확인 해
	// status 에 기록 한다.
	// +kubebuilder:default=false
	// +optional
	SpikeAlertAssertion bool `json:"spikeAlertAssertion,omitempty"`

	// ScoreTrigger 가 설정 되면 controller 가 correlation 의 간섭 score snapshot 을 조회 해 victim 과
	// suspect 의 score 가 임계 이상 일 때 부하 run 을 트리거 한다. 이때 Schedule 은 직접 트리거 가 아닌
	// score 평가 poll 주기 로 해석 되며, 실제 run 은 score 조건 충족 시에만 발생 한다. nil 이면 기존
	// 처럼 Schedule 따른 cron 트리거 로 동작 한다.
	// +optional
	ScoreTrigger *ScoreTriggerSpec `json:"scoreTrigger,omitempty"`

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

// ScoreTriggerSpec 는 correlation 간섭 score 기반 자동 트리거 조건 이다. controller 가 correlation
// exporter 의 noisy-neighbor snapshot 을 조회 해 (VictimRef, SuspectRef, Dimension) 매칭 페어 의 최대
// score 가 ScoreThreshold 이상 이면 부하 run 을 트리거 한다. 자동 트리거 도 기존 cron 경로 와 동일 한
// safety gate (동시성 락 / maxFailures suspend / prod 차단) 를 거친다.
type ScoreTriggerSpec struct {
	// VictimRef 는 간섭 을 받는 victim Pod 다. score snapshot 의 victim 식별자 와 매칭 된다.
	// +kubebuilder:validation:Required
	VictimRef LoadScenarioTargetRef `json:"victimRef"`

	// SuspectRef 는 간섭 을 일으키는 suspect Pod 다. 비우면 spec.targetRef (부하 대상) 를 suspect 로
	// 사용 해, suspect 를 stress 해 victim 저하 를 검증 하는 인과 루프 를 형성 한다.
	// +optional
	SuspectRef *LoadScenarioTargetRef `json:"suspectRef,omitempty"`

	// Dimension 은 score 를 평가 할 자원 차원 필터 다. 비우면 모든 차원 중 최대 score 를 본다.
	// +kubebuilder:validation:Enum=cpu;memory;network;gpu
	// +optional
	Dimension string `json:"dimension,omitempty"`

	// ScoreThreshold 는 트리거 임계 score 다 (0~1 사이 실수 문자열, 예 "0.7"). 매칭 페어 의 최대
	// score 가 본 값 이상 이면 트리거 한다. correlation score 는 0~1 정규화 상관 강도 다. CRD 의 float
	// 직렬화 비권장 을 피해 문자열 로 받고 controller 가 0~1 범위 를 검증 한다.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ScoreThreshold string `json:"scoreThreshold"`

	// MinInterval 은 score 트리거 run 사이 의 최소 간격 (debounce) 이다. score 가 임계 를 계속 넘더라도
	// 본 간격 이내 에는 재트리거 하지 않아 과도한 반복 인가 를 막는다. 기본값 10m.
	// +kubebuilder:default="10m"
	// +optional
	MinInterval metav1.Duration `json:"minInterval,omitempty"`
}

// LoadScenarioRunState 는 reconciler lifecycle state machine 의 상태값 이다.
// +kubebuilder:validation:Enum=Idle;Running;AwaitingSpikeAlert
type LoadScenarioRunState string

const (
	// RunStateIdle 은 현재 진행 중 인 부하 run 이 없는 상태다. 다음 schedule 시각 까지 wait 한다.
	RunStateIdle LoadScenarioRunState = "Idle"
	// RunStateRunning 은 stress Pod 가 spawn 되어 부하 가 인가 중 인 상태다. spec.duration 경과 후
	// reconciler 가 cleanup 단계 로 전환 한다.
	RunStateRunning LoadScenarioRunState = "Running"
	// RunStateAwaitingSpikeAlert 는 부하 종료 후 spike alert 자동 검증 polling 단계 의 상태다.
	// spec.spikeAlertAssertion 이 true 일 때만 진입 하며 polling window (기본 5 분) 경과 후 Idle 로
	// 전환 한다. polling 자체 는 매 reconcile 호출 시 1 회 단일 query 로 처리 되어 reconcile worker
	// 의 blocking 시간 을 짧게 유지 한다.
	RunStateAwaitingSpikeAlert LoadScenarioRunState = "AwaitingSpikeAlert"
)

// LoadScenarioStatus 는 controller 가 reconcile 결과 를 기록 하는 영역이다.
type LoadScenarioStatus struct {
	// RunState 는 reconciler lifecycle state machine 상태 이다. 비동기 reconcile 흐름 에서 stress
	// Pod 생성 / 종료 시점 분리 의 신호 로 사용 한다. 비어 있으면 Idle 로 간주.
	// +optional
	RunState LoadScenarioRunState `json:"runState,omitempty"`

	// RunStartTime 은 현재 진행 중 인 run 의 시작 시각 이다. RunState=Running 시점 에 set 되며
	// time.Now() >= RunStartTime + spec.duration 일 때 reconciler 가 cleanup 단계로 전환 한다.
	// +optional
	RunStartTime *metav1.Time `json:"runStartTime,omitempty"`

	// LastScheduleTime 은 controller 가 마지막 으로 run 을 트리거 한 시각 이다.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulRunTime 은 마지막 으로 정상 종료 된 run 의 시각 이다.
	// +optional
	LastSuccessfulRunTime *metav1.Time `json:"lastSuccessfulRunTime,omitempty"`

	// LastScoreTriggerTime 은 scoreTrigger 가 마지막 으로 run 을 트리거 한 시각 이다. minInterval
	// debounce 판정 의 기준 시각 으로 사용 되며, run 성공/실패 와 무관 하게 트리거 시점 에 기록 된다.
	// +optional
	LastScoreTriggerTime *metav1.Time `json:"lastScoreTriggerTime,omitempty"`

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
	//   - ScoreTriggered (scoreTrigger 설정 시 마지막 score 평가 의 트리거 충족 여부)
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
