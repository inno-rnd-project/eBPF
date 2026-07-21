package metadata

import "golang.org/x/sys/unix"

// Cgroup2Mounted 는 root 가 cgroup2 파일시스템인지 statfs magic 으로 판정한다 (#297). cgroup id
// 역매핑 스캐너 (#228) 는 "cgroup id == 디렉터리 inode" 동일성이라는 cgroup2 전제에 의존하므로,
// v1/hybrid 노드에서는 호출부가 스캐너를 기동하지 않고 사유를 로그와 netobs_cgroup2_available
// gauge 로 노출한다. root 미존재 (로컬 실행 등 mount 부재) 는 error 로 구분된다.
func Cgroup2Mounted(root string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return false, err
	}
	return st.Type == unix.CGROUP2_SUPER_MAGIC, nil
}
