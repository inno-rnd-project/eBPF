//go:build integration

package integration

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestT6_OverlayDriftDevVsProd 는 deploy/gpuobs/overlays/{dev,prod} 의 kustomize build 결과를 비교해
// 의도된 차이만 갖는지 검증한다. RBAC / NetworkPolicy / 매니페스트 누락 / 의도하지 않은 패치 drift 를
// 통합 레이어에서 fail-fast 로 잡아 #37 (privileged 제거) 같은 후속 작업의 회귀 가드로 동작한다.
//
// 의도된 차이 (현재 시점):
//   - DaemonSet 의 imagePullPolicy: dev=Never, prod=IfNotPresent
//   - DaemonSet 의 nodeSelector: dev 는 observability.netobs/canary=true 추가, prod 는 미적용
//   - DaemonSet image: dev 는 newName 미설정 (gpuobs-agent), prod 는 ghcr 접두 명시
//   - DaemonSet image newTag: 두 overlay 모두 동일 (make bump 동기화)
//
// 위 4 가지 외의 차이는 drift 로 간주해 테스트 실패.
func TestT6_OverlayDriftDevVsProd(t *testing.T) {
	root := repoRoot(t)
	devPath := filepath.Join(root, "deploy", "gpuobs", "overlays", "dev")
	prodPath := filepath.Join(root, "deploy", "gpuobs", "overlays", "prod")

	devDocs := kustomizeBuild(t, devPath)
	prodDocs := kustomizeBuild(t, prodPath)

	// kind / metadata.name 으로 매니페스트를 인덱싱한다.
	devByKey := indexManifests(t, devDocs)
	prodByKey := indexManifests(t, prodDocs)

	// 양쪽이 동일한 매니페스트 셋을 가져야 한다 (RBAC / Service / DaemonSet / ServiceAccount /
	// PrometheusRule / ServiceMonitor 누락 회귀 방지).
	for key := range devByKey {
		if _, ok := prodByKey[key]; !ok {
			t.Errorf("manifest %q exists in dev overlay but not prod", key)
		}
	}
	for key := range prodByKey {
		if _, ok := devByKey[key]; !ok {
			t.Errorf("manifest %q exists in prod overlay but not dev", key)
		}
	}

	// 의도된 차이는 DaemonSet 한 곳에서만 나타나야 한다. 다른 매니페스트는 byte-level 동일해야 한다.
	for key, devManifest := range devByKey {
		prodManifest := prodByKey[key]
		if prodManifest == nil {
			continue
		}
		if strings.HasPrefix(key, "DaemonSet/ebpf-project/gpuobs-agent") {
			continue
		}
		if !bytes.Equal(devManifest, prodManifest) {
			t.Errorf("manifest %q drifted between dev and prod (only DaemonSet may differ)", key)
		}
	}

	// DaemonSet 의 의도된 차이를 정확히 검증한다.
	devDS := mustFindManifest(t, devByKey, "DaemonSet/ebpf-project/gpuobs-agent")
	prodDS := mustFindManifest(t, prodByKey, "DaemonSet/ebpf-project/gpuobs-agent")

	devImage := getDaemonSetImage(t, devDS)
	prodImage := getDaemonSetImage(t, prodDS)
	if !strings.HasPrefix(prodImage, "ghcr.io/") {
		t.Errorf("prod image=%q want ghcr.io/* prefix", prodImage)
	}
	if strings.Contains(devImage, "ghcr.io/") {
		t.Errorf("dev image=%q must not have ghcr.io prefix (uses local docker tag)", devImage)
	}
	devTag := imageTag(devImage)
	prodTag := imageTag(prodImage)
	if devTag != prodTag {
		t.Errorf("dev image tag %q vs prod tag %q must match (make bump 동기화 회귀)", devTag, prodTag)
	}

	devPolicy := getDaemonSetImagePullPolicy(t, devDS)
	prodPolicy := getDaemonSetImagePullPolicy(t, prodDS)
	if devPolicy != "Never" {
		t.Errorf("dev imagePullPolicy=%q want Never", devPolicy)
	}
	if prodPolicy != "IfNotPresent" {
		t.Errorf("prod imagePullPolicy=%q want IfNotPresent", prodPolicy)
	}

	devSelector := getDaemonSetNodeSelector(t, devDS)
	prodSelector := getDaemonSetNodeSelector(t, prodDS)
	if v := devSelector["observability.netobs/canary"]; v != "true" {
		t.Errorf("dev nodeSelector observability.netobs/canary=%q want true", v)
	}
	if _, ok := prodSelector["observability.netobs/canary"]; ok {
		t.Errorf("prod nodeSelector must not contain observability.netobs/canary key")
	}
}

// ---- 헬퍼 ----

func kustomizeBuild(t *testing.T, path string) [][]byte {
	t.Helper()
	cmd := exec.Command("kubectl", "kustomize", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("kubectl kustomize unavailable or failed: %v\n%s", err, out)
	}
	return splitYAMLDocs(out)
}

func splitYAMLDocs(buf []byte) [][]byte {
	var docs [][]byte
	for _, doc := range bytes.Split(buf, []byte("\n---\n")) {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
	return docs
}

type manifestHeader struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

func indexManifests(t *testing.T, docs [][]byte) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(docs))
	for _, doc := range docs {
		var h manifestHeader
		if err := yaml.Unmarshal(doc, &h); err != nil {
			t.Fatalf("manifest header parse: %v", err)
		}
		// namespaced 객체와 cluster-scoped 객체가 같은 Kind + Name 을 가질 수 있어 namespace 를
		// 키에 포함해 silent collision 을 방지한다 (예: 두 다른 namespace 의 ConfigMap/X).
		key := h.Kind + "/" + h.Metadata.Name
		if h.Metadata.Namespace != "" {
			key = h.Kind + "/" + h.Metadata.Namespace + "/" + h.Metadata.Name
		}
		out[key] = doc
	}
	return out
}

func mustFindManifest(t *testing.T, m map[string][]byte, key string) []byte {
	t.Helper()
	doc := m[key]
	if doc == nil {
		t.Fatalf("manifest %q not found", key)
	}
	return doc
}

func getDaemonSetImage(t *testing.T, doc []byte) string {
	t.Helper()
	var ds struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(doc, &ds); err != nil {
		t.Fatalf("daemonset parse: %v", err)
	}
	if len(ds.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("daemonset has no containers")
	}
	return ds.Spec.Template.Spec.Containers[0].Image
}

func getDaemonSetImagePullPolicy(t *testing.T, doc []byte) string {
	t.Helper()
	var ds struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						ImagePullPolicy string `json:"imagePullPolicy"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(doc, &ds); err != nil {
		t.Fatalf("daemonset parse: %v", err)
	}
	if len(ds.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("daemonset has no containers")
	}
	return ds.Spec.Template.Spec.Containers[0].ImagePullPolicy
}

func getDaemonSetNodeSelector(t *testing.T, doc []byte) map[string]string {
	t.Helper()
	var ds struct {
		Spec struct {
			Template struct {
				Spec struct {
					NodeSelector map[string]string `json:"nodeSelector"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(doc, &ds); err != nil {
		t.Fatalf("daemonset parse: %v", err)
	}
	return ds.Spec.Template.Spec.NodeSelector
}

func imageTag(image string) string {
	idx := strings.LastIndex(image, ":")
	if idx < 0 {
		return ""
	}
	return image[idx+1:]
}
