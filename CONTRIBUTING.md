# 기여 가이드

이 문서는 코드/문서 기여 시 따라야 할 컨벤션과 절차를 정리한 것이며, 프로젝트의 기존 파일들이 해당 규칙의 기준 예시이다.

## 작성 언어

커밋 메시지, 이슈 본문, PR 설명, README/문서, 코드 주석 등 프로젝트의 텍스트 산출물은 **한국어를 기본 언어**로 사용한다. 영문으로 작성된 기여도 수용하지만 메인테이너가 한국어로의 변환을 요청할 수 있으며, 함수명/설정 키/약어 등 기술 식별자는 가능한 한 영문을 유지한다.

한국어 작성 시에는 다음 규칙을 따른다.

- 완결된 문장 형태로 작성할 때는 `~한다` / `~이다` 종결어미를 사용하고, 문장을 잘게 쪼개지 않도록 쉼표와 연결어미(`~하며`, `~하고` 등)로 자연스럽게 이어 쓴다
- 불릿(`-`)으로 항목을 구분할 때는 마침표를 붙이지 않으며, 단순 항목/구성 요소 나열은 명사형 어미(예: "~ 확인", "~ 검증", "~ 처리")를, 동작 규칙이나 행동 지침 서술은 `~한다` 종결을 사용한다
- 기술 용어는 원문을 유지하며, 필요 시 괄호로 한국어 부연을 덧붙인다

## 개발 환경

이 프로젝트는 Linux 커널 eBPF 프로그램과 Go 에이전트로 구성되므로, 다음 도구가 준비되어야 한다.

- Go 1.22 이상
- clang (BPF 컴파일)
- bpftool (`vmlinux.h` 생성)
- BTF가 활성화된 Linux 커널 (5.2 이상 권장)
- Docker 및 Kubernetes 클러스터 (배포 검증 시)

의존성 초기 설정과 빌드는 `Makefile`에 정리되어 있으며, `make build`는 `go fmt`, BPF 코드 재생성, Go 바이너리 빌드를 한 번에 수행한다.

## 커밋 메시지 규약

커밋 메시지는 [Conventional Commits](https://www.conventionalcommits.org/) 형식을 따른다. 헤더는 `type(scope): subject` 구조이며, 한 커밋에 여러 영역이 걸치는 경우 scope는 생략할 수 있다.

사용하는 type은 다음과 같다.

- `feat`: 새 기능 추가
- `fix`: 버그 수정 또는 동작 보정
- `refactor`: 동작 변화 없는 구조 개선
- `docs`: 문서 변경
- `build`: 빌드, 이미지, 릴리스 관련
- `test`: 테스트 추가/수정

subject는 무엇이 바뀌었는지 요약한 한국어 문장 또는 구로 쓰며, 마침표 없이 끝낸다. 본문에는 변경 이유와 배경을 `~한다` 종결로 이어서 서술하고, 파일 단위의 세부 내역은 불릿으로 정리한다.

예시는 다음과 같다.

```
fix: flow 캐시 메모리 주석 보정 및 이벤트 로그 기본값 비활성화

flow 캐시의 peak 메모리 주석을 실측 수치에 맞춰 보정하고, PrintEvents 설정의 기본값을 true에서 false로 변경한다. 기본 실행 시 stdout 로그가 과도하게 쏟아지지 않도록 한다.

- `internal/metadata/resolver.go`: peak 메모리 주석을 "~60MB"에서 "~200MB"로 수정
- `internal/config/config.go`: PrintEvents 기본값을 false로 변경
```

## Go 코드 컨벤션

`cmd/netobs-agent/main.go`와 `internal/` 하위 패키지들이 기준 예시이다. 스타일은 `gofmt` 출력을 기본으로 하며, `make build`에 `go fmt ./...`가 포함되어 있다.

### 패키지와 디렉토리 구조

- 바이너리 진입점은 `cmd/<binary-name>/main.go`에 둔다
- 구현은 `internal/<domain>/`에 도메인 별 패키지로 분리한다 (예: `metadata`, `metrics`, `config`, `drop`, `server`, `types`, `ebpf`)
- 패키지명은 디렉토리명과 일치시키며, 외부 라이브러리와 충돌하는 경우에만 import 시점에서 alias를 준다 (예: `ebpfx "netobs/internal/ebpf"`)

### Import 그룹

`import` 블록은 stdlib, 외부 라이브러리, 프로젝트 내부 순서로 **빈 줄로 구분된 세 그룹**으로 쓴다. 이 순서는 `goimports`, `gci` 등 Go 표준 포맷터의 기본과 일치하며, 들여쓰기는 gofmt 가 출력하는 **탭**을 그대로 사용한다.

```go
import (
	"context"
	"errors"
	"log"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/config"
	"netobs/internal/metadata"
)
```

### 네이밍과 식별자

- 외부로 노출되는 타입, 함수, 상수는 `PascalCase`로 쓴다
- 패키지 내부 식별자는 `camelCase`로 쓴다
- 의미 있는 축약이 아닌 이상 1~2글자 변수는 지양한다
- Kubernetes/eBPF 도메인 용어는 원문을 유지한다 (예: `PodIdentity`, `CgroupID`, `SkbIif`)

### 에러 처리

- 에러는 호출 경로 위로 전달하는 것을 기본으로 하며, `main()`에서만 `log.Fatalf`로 종료한다
- 라이브러리성 패키지 내부에서는 `log.Fatal*` 호출을 피하고 에러를 반환한다
- 래핑이 필요하면 `fmt.Errorf("context: %w", err)` 또는 `fmt.Errorf("context: %v", err)`를 사용한다
- 사용자 상호작용과 무관한 예외 상황은 `log.Printf`로 남긴다

### 동시성

- 공유 상태 접근은 `sync.RWMutex` + `defer r.mu.RUnlock()` 패턴을 기본으로 한다
- 단순 플래그는 `atomic.Bool` 등 원자 타입을 쓰고, 세터가 startup 시점에만 호출되는 경우 그 조건을 주석으로 명시한다
- 종료 경로는 `context.Context`를 통해 전파하고, goroutine은 `ctx.Done()` 또는 채널 close를 종료 신호로 삼는다

### 설정과 튜닝 값

- 튜닝 가능한 값은 패키지 상수가 아니라 **struct field**로 두고 생성자에서 기본값을 초기화한다 (예: `flowRotateEvery`, `flowMaxCurrent`)
- 외부에서 조정이 필요한 값은 `internal/config`의 `Config` 구조체 필드로 노출하고, env와 CLI flag 양쪽에서 받을 수 있게 한다
- 기본 동작은 "조용하고 안전한 쪽"을 고르되, 구체 기본값은 코드와 배포 매니페스트, README의 Configuration 표가 단일 진실원으로 일치하도록 유지한다

### 주석

- 한국어 주석을 기본으로 하며, exported 식별자에 대한 godoc은 필요 시 한국어로 작성한다
- 주석은 "무엇을"이 아닌 "왜"를 설명하는 것을 우선으로 한다
- 수치/상한 등 구체 값을 주석에 적을 때는 계산 근거를 함께 남긴다 (예: `flowCacheEntry ~1KB × 100,000 × 2 ≈ ~200MB`)

## C / eBPF 코드 컨벤션

`bpf/common.h`, `bpf/netlat.bpf.c`가 기준 예시이다.

### 파일 구조와 헤더

- 첫 블록은 프로젝트 내부 헤더(`vmlinux.h`, `common.h`), 이어서 `<bpf/...>` 헤더를 빈 줄로 구분해 include 한다
- 라이선스는 BPF 로더가 요구하므로 반드시 명시한다: `char LICENSE[] SEC("license") = "GPL";`
- struct 및 enum 공유 정의는 `bpf/common.h`에 두고, Go 쪽 `types.Event`와 필드 offset이 일치하도록 유지한다

### 맵과 섹션

- 맵 정의는 anonymous struct + `SEC(".maps")` 형식을 쓴다
- kprobe/kretprobe 프로그램은 `SEC("kprobe/<symbol>")` 형식으로 선언하고, 함수는 `BPF_KPROBE` / `BPF_KRETPROBE` 매크로로 감싼다
- 이벤트 전달은 ringbuf를 우선 사용한다

### 커널 필드 접근

- 커널 구조체 필드는 `BPF_CORE_READ(obj, field.path)`로 읽어 CO-RE relocation 적용을 받는다
- 같은 파일 내에서 필드 접근 방식은 일관성을 우선한다 (예: `__sk_common.skc_*` 필드는 모두 `BPF_CORE_READ`로 통일)
- helper 함수(`bpf_*`)는 런타임 서비스 성격(현재 pid/cgroup, 시간, 맵 통신 등)에 한해 사용한다

### 들여쓰기와 코드 스타일

- 4 스페이스 들여쓰기를 쓴다
- 반복되는 필드 읽기는 `static __always_inline` 헬퍼로 묶어 재사용한다
- 주석은 한국어를 기본으로 하며, kernel ABI나 비정상 동작에 대한 배경은 주석으로 명시한다

## Kubernetes 배포 자원

`deploy/base/`와 `deploy/overlays/`의 Kustomize 구조를 따른다.

- 공통 자원은 `base/`에, 환경별 차이는 `overlays/<env>/`에 patch로 둔다
- `NodeSelector`, `hostPID`, `privileged` 설정은 eBPF 로딩에 필요한 조합이며, 프로덕션 overlay에서는 `accelerator`/`observability.netobs/*` 라벨 등 대상 노드가 제한되어 있는지 확인한다
- 새 env 변수를 추가할 때는 `internal/config/config.go`, `deploy/base/daemonset.yaml`, `README.md`의 Configuration 표를 함께 갱신한다

## 버전 관리

버전은 프로젝트 루트의 `VERSION` 파일을 단일 진실원(single source of truth)으로 사용한다.

- 버전은 `make bump`으로만 증가시키며, 이 명령은 `VERSION`과 `deploy/overlays/*/kustomization.yaml`의 image tag를 함께 갱신한다
- 버전 변경은 별도 커밋으로 분리하고, 메시지는 `build(release): bump version to X.Y.Z` 형식을 유지한다

## Pull Request 절차

- 브랜치는 `feature/<topic>` 또는 `fix/<topic>` 형식으로 이름을 정한다
- PR 전에 `make build`로 `go fmt`와 빌드 통과 여부를 확인한다
- PR 본문에는 변경 목적, 주요 설계 결정, 운영 영향(메모리/카디널리티/최소 커널 등)을 한국어로 기술하고, 리뷰어가 이해할 수 있도록 배경을 충분히 제공한다
- 자동 코드 리뷰(Copilot, Gemini 등)의 지적에 대응할 때는 현재 코드 맥락과 규모에 맞춰 타당성을 검토한 뒤 수용/기각 근거를 PR 코멘트로 남긴다

## AI Assistant 활용

본 프로젝트는 AI Assistant 활용을 적극적으로 권장하며, 이슈사항 발생 시에는 인간의 적극적인 개입 및 해결을 원칙으로 한다.
