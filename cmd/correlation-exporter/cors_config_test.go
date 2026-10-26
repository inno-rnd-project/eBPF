package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// corsOriginsFrom 은 ConfigMap 매니페스트에서 API_CORS_ALLOW_ORIGINS 목록을 읽는다.
func corsOriginsFrom(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	v, ok := cm.Data["API_CORS_ALLOW_ORIGINS"]
	if !ok {
		t.Fatalf("%s 에 API_CORS_ALLOW_ORIGINS 없음", path)
	}
	var out []string
	for _, o := range strings.Split(v, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// TestCORSAllowListEnvSeparation 은 #461 의 환경 분리를 고정한다. base (prod 포함 전 환경) 목록에는
// 개발 접속 경로 (localhost, 127.0.0.1, 사설 IP 개발 서버) 가 없어야 하고, dev patch 는 base 목록을
// 다시 적는 구조라 base 의 모든 origin 을 포함해야 한다. base 에 origin 을 추가하고 dev patch 갱신을
// 빠뜨리면 dev 에서만 그 경로가 거부되는 회귀를 막는다.
func TestCORSAllowListEnvSeparation(t *testing.T) {
	base := corsOriginsFrom(t, "../../deploy/correlation/base/configmap.yaml")
	dev := corsOriginsFrom(t, "../../deploy/correlation/overlays/dev/patch-configmap.yaml")

	for _, o := range base {
		host := strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://")
		if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.") ||
			strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "10.") {
			t.Errorf("base CORS 목록에 개발 접속 경로 %q 가 있다 (dev overlay 로 옮길 것)", o)
		}
	}
	devSet := map[string]bool{}
	for _, o := range dev {
		devSet[o] = true
	}
	for _, o := range base {
		if !devSet[o] {
			t.Errorf("dev patch CORS 목록에 base origin %q 누락 (base 와 함께 갱신할 것)", o)
		}
	}
	if !devSet["http://localhost:3000"] {
		t.Errorf("dev patch CORS 목록에 port-forward 경로 http://localhost:3000 이 없다")
	}
}
