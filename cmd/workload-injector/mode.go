package main

import "strings"

// parseModeFlag 는 main flag.Parse 전에 -mode 값을 peek 한다. CLI mode 와 controller mode 가 동일
// binary 안에서 분기 되므로 loadConfig 의 flag 파싱 전에 mode 를 결정 해야 controller mode 가 CLI
// 전용 필수 flag (target-pod 등) 의 부재로 fatal exit 되지 않는다. flag.FlagSet 을 2 번 파싱 하면
// duplicate flag panic 이 발생 하므로 args 를 직접 스캔 한다. mode 값 외 flag 는 본 함수가 소비
// 하지 않아 loadConfig 가 그대로 다시 파싱 가능 하다.
func parseModeFlag(args []string) string {
	const prefix = "-mode"
	for i, a := range args {
		switch {
		case a == prefix || a == "--mode":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, prefix+"=") || strings.HasPrefix(a, "--mode="):
			return strings.SplitN(a, "=", 2)[1]
		}
	}
	return "cli"
}
