//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// startEnvtest 는 controller-runtime envtest 를 부팅해 in-process kube-apiserver + etcd 를 띄우고,
// kubernetes.Interface 와 cleanup 함수를 반환한다. 통합 테스트가 envtest binary (etcd / kube-apiserver)
// 의 setup-envtest 캐시 경로를 KUBEBUILDER_ASSETS 환경변수로 받아 사용한다는 가정에 의존한다.
//
// 부팅 실패 분기는 KUBEBUILDER_ASSETS 설정 여부에 따라 다르게 처리한다. CI 처럼 자산이 사전 준비된
// 환경에서 부팅이 실패하면 apiserver / etcd 회귀가 발생한 상황이므로 t.Fatalf 로 즉시 fail 시킨다.
// 자산이 비어 있으면 binary 가 없는 로컬 환경이므로 t.Skip 으로 자연 폴백한다. 자산 설정 여부와
// 무관하게 모두 skip 으로 처리하면 회귀가 silent green 으로 통과해 머지 게이트 의미가 사라진다.
func startEnvtest(t *testing.T) (kubernetes.Interface, *rest.Config, func()) {
	t.Helper()
	env := &envtest.Environment{
		ErrorIfCRDPathMissing: false,
	}
	cfg, err := env.Start()
	if err != nil {
		if assets := os.Getenv("KUBEBUILDER_ASSETS"); assets != "" {
			t.Fatalf("envtest start failed despite KUBEBUILDER_ASSETS=%s: %v", assets, err)
		}
		t.Skipf("envtest unavailable (run `make test-integration` to set up): %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		_ = env.Stop()
		t.Fatalf("kube clientset: %v", err)
	}
	cleanup := func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}
	return cs, cfg, cleanup
}

// repoRoot 는 본 파일의 위치를 기준으로 리포지토리 루트 절대 경로를 산출한다. CRD / manifest 경로
// 참조가 필요할 때 (kustomize overlay 검증 등) 사용한다. 본 패키지 위치 (internal/gpuobs/integration)
// 에서 세 단계 상위가 루트다.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
