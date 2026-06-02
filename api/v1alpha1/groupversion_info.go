// Package v1alpha1 는 #102 의 LoadScenario CRD 타입 그룹을 정의한다. injector.netobs.io/v1alpha1
// API group 으로 controller-runtime scheme 에 등록되어 LoadScenario reconciler 가 client 를 통해
// LoadScenario 객체를 조작 가능하게 한다.
//
// +kubebuilder:object:generate=true
// +groupName=injector.netobs.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion 은 본 패키지가 노출하는 API group 과 version 식별자다.
var GroupVersion = schema.GroupVersion{Group: "injector.netobs.io", Version: "v1alpha1"}

// SchemeBuilder 는 controller-runtime scheme 에 본 group 의 타입을 등록하기 위한 헬퍼다.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme 는 controller manager 가 scheme 에 LoadScenario 와 LoadScenarioList 를 등록할 때
// 호출한다. cmd/workload-injector 의 controller mode 에서 manager bootstrap 단계에 사용된다.
var AddToScheme = SchemeBuilder.AddToScheme
