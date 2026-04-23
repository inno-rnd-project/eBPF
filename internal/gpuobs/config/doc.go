// Package config는 gpuobs 에이전트의 실행 시 설정을 정의한다.
// env와 CLI flag 양쪽에서 값을 받을 수 있으며, CLI flag가 env를 덮어쓴다.
// Phase 1에서는 ListenAddr과 NodeName 두 항목만 노출하며, 폴링 주기와
// 카디널리티 토글 등은 Phase 2 이후에 추가된다.
package config
