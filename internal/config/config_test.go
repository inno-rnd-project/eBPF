package config

import (
	"flag"
	"os"
	"testing"
	"time"
)

// prepareParseTest은 flag.CommandLine을 격리된 FlagSet으로 교체하고
// 테스트 종료 시 원래 상태로 복원한다. config.Parse() 를 여러 테스트에서
// 독립적으로 호출할 수 있도록 한다.
func prepareParseTest(t *testing.T) {
	t.Helper()
	orig := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	t.Cleanup(func() { flag.CommandLine = orig })
}

// -------------------------------------------------------------------
// getenv
// -------------------------------------------------------------------

func TestGetenv(t *testing.T) {
	t.Setenv("_TEST_GETENV_KEY", "hello")
	if got := getenv("_TEST_GETENV_KEY", "default"); got != "hello" {
		t.Errorf("getenv(set) = %q, want %q", got, "hello")
	}

	t.Setenv("_TEST_GETENV_KEY", "  ")
	if got := getenv("_TEST_GETENV_KEY", "default"); got != "default" {
		t.Errorf("getenv(whitespace-only) = %q, want default", got)
	}

	if got := getenv("_TEST_GETENV_UNSET_XYZ", "fallback"); got != "fallback" {
		t.Errorf("getenv(unset) = %q, want fallback", got)
	}
}

// -------------------------------------------------------------------
// getenvBool
// -------------------------------------------------------------------

func TestGetenvBool(t *testing.T) {
	trueVals := []string{"1", "true", "TRUE", "True", "yes", "YES", "y", "Y"}
	for _, v := range trueVals {
		t.Setenv("_TEST_BOOL", v)
		if got := getenvBool("_TEST_BOOL", false); !got {
			t.Errorf("getenvBool(%q) = false, want true", v)
		}
	}

	falseVals := []string{"0", "false", "FALSE", "no", "NO", "n", "N", "anything-else"}
	for _, v := range falseVals {
		t.Setenv("_TEST_BOOL", v)
		if got := getenvBool("_TEST_BOOL", true); got {
			t.Errorf("getenvBool(%q) = true, want false", v)
		}
	}

	// 미설정이면 기본값 반환
	t.Setenv("_TEST_BOOL", "")
	if got := getenvBool("_TEST_BOOL", true); !got {
		t.Errorf("getenvBool(empty) = false, want default=true")
	}
}

// -------------------------------------------------------------------
// getenvDuration
// -------------------------------------------------------------------

func TestGetenvDuration(t *testing.T) {
	t.Run("유효한 duration", func(t *testing.T) {
		t.Setenv("_TEST_DUR", "5m")
		d, err := getenvDuration("_TEST_DUR", time.Minute)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d != 5*time.Minute {
			t.Errorf("duration = %v, want 5m", d)
		}
	})

	t.Run("잘못된 duration 문자열 → 오류", func(t *testing.T) {
		t.Setenv("_TEST_DUR", "notaduration")
		_, err := getenvDuration("_TEST_DUR", time.Minute)
		if err == nil {
			t.Error("invalid duration: 오류 기대, nil 반환됨")
		}
	})

	t.Run("미설정 → 기본값 반환", func(t *testing.T) {
		t.Setenv("_TEST_DUR", "")
		d, err := getenvDuration("_TEST_DUR", 30*time.Second)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d != 30*time.Second {
			t.Errorf("duration = %v, want 30s", d)
		}
	})
}

// -------------------------------------------------------------------
// Parse: 유효성 검증 에러 경로
// -------------------------------------------------------------------

func TestParseInvalidTargetIP(t *testing.T) {
	prepareParseTest(t)
	t.Setenv("TARGET_IP", "not-an-ip")
	t.Setenv("KUBE_METADATA_REFRESH", "30s")

	_, err := Parse()
	if err == nil {
		t.Error("유효하지 않은 TARGET_IP: 오류 기대, nil 반환됨")
	}
}

func TestParseInvalidMetadataRefresh(t *testing.T) {
	prepareParseTest(t)
	t.Setenv("TARGET_IP", "")
	t.Setenv("KUBE_METADATA_REFRESH", "-1m")

	_, err := Parse()
	if err == nil {
		t.Error("음수 KUBE_METADATA_REFRESH: 오류 기대, nil 반환됨")
	}
}

func TestParseZeroMetadataRefresh(t *testing.T) {
	prepareParseTest(t)
	t.Setenv("TARGET_IP", "")
	t.Setenv("KUBE_METADATA_REFRESH", "0s")

	_, err := Parse()
	if err == nil {
		t.Error("0s KUBE_METADATA_REFRESH: 오류 기대, nil 반환됨")
	}
}

func TestParseInvalidDurationEnv(t *testing.T) {
	prepareParseTest(t)
	t.Setenv("KUBE_METADATA_REFRESH", "notaduration")

	_, err := Parse()
	if err == nil {
		t.Error("잘못된 KUBE_METADATA_REFRESH 형식: 오류 기대, nil 반환됨")
	}
}

func TestParseValidConfig(t *testing.T) {
	prepareParseTest(t)
	t.Setenv("TARGET_IP", "10.0.0.1")
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("KUBE_METADATA_REFRESH", "1m")
	t.Setenv("POD_METRICS_ENABLED", "false")
	t.Setenv("NODE_NAME", "test-node")

	cfg, err := Parse()
	if err != nil {
		t.Fatalf("유효한 설정: 오류 없어야 함, got: %v", err)
	}
	if cfg.TargetIP != "10.0.0.1" {
		t.Errorf("TargetIP = %q, want %q", cfg.TargetIP, "10.0.0.1")
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":9999")
	}
	if cfg.MetadataRefresh != time.Minute {
		t.Errorf("MetadataRefresh = %v, want 1m", cfg.MetadataRefresh)
	}
	if cfg.PodMetricsEnabled {
		t.Error("PodMetricsEnabled = true, want false")
	}
	if cfg.NodeName != "test-node" {
		t.Errorf("NodeName = %q, want %q", cfg.NodeName, "test-node")
	}
}
