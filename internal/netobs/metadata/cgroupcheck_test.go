package metadata

import "testing"

// TestCgroup2Mounted 는 #297 의 statfs 판정을 검증한다. 일반 파일시스템 (tempdir) 은 cgroup2 가
// 아니라 false 이고, 미존재 경로는 error 로 구분된다 (mount 부재 케이스). 양성 케이스는 커널
// cgroup 구성에 의존해 환경 종속이므로 dev 실배포에서 gauge 로 확인한다.
func TestCgroup2Mounted(t *testing.T) {
	ok, err := Cgroup2Mounted(t.TempDir())
	if err != nil || ok {
		t.Errorf("tempdir: ok=%v err=%v want false/nil", ok, err)
	}
	ok, err = Cgroup2Mounted("/이런/경로/없음")
	if err == nil || ok {
		t.Errorf("미존재 경로: ok=%v err=%v want false/error", ok, err)
	}
}
