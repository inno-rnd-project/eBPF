# observability-agent

Kubernetes observability agent suite combining eBPF-based network latency tracing (`netobs`), NVML-based GPU state collection (`gpuobs`), and a Pearson correlation Top-N noisy neighbor exporter (`correlation-exporter`). The two agents are deployed as DaemonSets on observed nodes; correlation-exporter is a cluster-wide singleton Deployment that consumes the agents' Prometheus metrics and emits Top-N noisy neighbor series.

## Components

| Component | Kind | Listen | Source | Role |
|---|---|---|---|---|
| `netobs-agent` | DaemonSet | `:9810` | [`cmd/netobs-agent`](cmd/netobs-agent) | eBPF kprobe로 TCP 송신 stage latency, drop reason, retransmission을 측정해 `netobs_*` 메트릭 emit |
| `gpuobs-agent` | DaemonSet | `:9820` | [`cmd/gpuobs-agent`](cmd/gpuobs-agent) | NVML / CUDA uprobe로 GPU 디바이스 상태, GPU별 메모리 점유, CUDA kernel launch / memcpy를 측정해 `gpuobs_*` 메트릭 emit |
| `correlation-exporter` | Deployment | `:9830` | [`cmd/correlation-exporter`](cmd/correlation-exporter), [`internal/correlation`](internal/correlation) | 주기적으로 `netobs_*` / `gpuobs_*` 시계열을 fetch해 Pearson 상관계수 산출 후 Top-N noisy neighbor를 `correlation_noisy_neighbor_*` 메트릭으로 emit |
| `correlation-debug` | CLI (one-shot) | n/a | [`cmd/correlation-debug`](cmd/correlation-debug) | exporter와 동일 라이브러리를 reuse하는 운영자용 1회성 진단 CLI. alert label로 받은 페어를 같은 시점에 재현 |
| `workload-injector` | Job | `:9840` | [`cmd/workload-injector`](cmd/workload-injector), [`internal/injector`](internal/injector) | dev / staging 환경에서 cpu / network / gpu 합성 부하를 트리거해 correlation 분석 layer 의 산출을 검증. `injector_active` 와 `correlation_blast_radius_score` 메트릭 emit. prod 자동 실행 금지 (cluster label gate) |

각 컴포넌트의 상세 동작과 추가 환경변수는 본 문서의 해당 절과 [`internal/correlation/README.md`](internal/correlation/README.md) 를 참고한다.

## Prerequisites

Shared:
- Go 1.24+
- Linux kernel with BTF support (required by netobs)

For netobs (network observer):
- clang (BPF compilation)
- bpftool (vmlinux.h generation)

For gpuobs (GPU observer):
- Target node has NVIDIA GPU Operator or `nvidia-container-runtime` installed
- `libnvidia-ml.so.1` injectable at runtime (triggered by `NVIDIA_VISIBLE_DEVICES` env)

## Local build

```bash
make deps
make build-netobs-agent          # netobs-agent binary (runs BPF regeneration first)
make build-gpuobs-agent          # gpuobs-agent binary
make build-correlation-debug     # one-shot diagnosis CLI (no image)
make build-correlation-exporter  # cluster Deployment binary
make build-all                   # every deployable binary (agents + correlation-exporter)
```

## Local run

```bash
# netobs needs root for BPF loading
sudo ./bin/netobs-agent -listen :9810 -print-events=true

# gpuobs does not need root
./bin/gpuobs-agent -listen :9820

# correlation-debug — Prometheus port-forward 후 직전 1시간 윈도우 산출
kubectl port-forward -n monitoring svc/kube-prometheus-stack-prometheus 9090:9090 &
./bin/correlation-debug -prometheus-url http://localhost:9090 -window 1h

# correlation-exporter — cluster 외부 디버그 실행 (운영 배포는 deploy-correlation-{dev,prod} 사용)
./bin/correlation-exporter -prometheus-url http://localhost:9090 -listen :9830
```

## Tests

Two test layers run in this repo. Unit tests run via `make test`, which regenerates the bpf2go embed artifacts (`internal/netobs/ebpf/*.o`, `internal/gpuobs/cuda/cudauprobe_*.o`) before invoking `go test -race ./...`; the artifacts are gitignored, so a raw `go test ./...` on a fresh checkout fails to compile until `make generate` and `make generate-gpuobs` have run. Integration tests live under [`internal/gpuobs/integration/`](internal/gpuobs/integration/) behind a `//go:build integration` tag and verify cross-package behavior (kube.Resolver informer ordering, NVML refresh cycle consistency, dispatch hot path branches, opt-out toggle effects, `/metrics` Prometheus text format, kustomize overlay drift) without requiring real BPF kernel attach or real NVIDIA GPUs.

```bash
# Unit tests (regenerates BPF artifacts as prereq, race detector always on)
make test

# Integration tests (sets up envtest binaries via setup-envtest, race detector on, 60s ceiling)
make test-integration
```

The CI workflow [`integration.yml`](.github/workflows/integration.yml) runs the integration suite on every PR and main push, gating merges to main once the corresponding branch protection rule is enabled. Real BPF kernel attach, real multi-GPU node attribution, and DCGM exporter cross-validation are explicitly out of scope for the integration layer and tracked as separate follow-up issues that require self-hosted runners or a multi-GPU environment.

## Configuration

### netobs-agent

| Environment Variable | CLI Flag | Default | Description |
|---|---|---|---|
| `TARGET_IP` | `-target-ip` | *(empty, trace all)* | Target Pod IPv4 to trace |
| `LISTEN_ADDR` | `-listen` | `:9810` | HTTP listen address |
| `PRINT_EVENTS` | `-print-events` | `false` | Print events to stdout |
| `POD_METRICS_ENABLED` | `-pod-metrics` | `true` | Emit per-pod-instance metrics (`netobs_pod_stage_*`); disable on large clusters to cap Prometheus cardinality |
| `POD_FLOW_DST_ENABLED` | `-pod-flow-dst` | `true` | Emit `dst_namespace`/`dst_workload` labels on stage/drop/retrans metrics; disable to keep pre-flow-dst cardinality |
| `POD_FLOW_DST_UID_ALLOW_NAMESPACES` | `-pod-flow-dst-uid-allow-namespaces` | *(empty)* | Comma-separated namespace allow-list whose Pods receive `dst_pod_uid` labels; empty disables UID emit cluster-wide. Each entry must match RFC1123 DNS label (lowercase alphanumeric + hyphen, 1-63 chars) and is fail-fast validated at startup |
| `NETOBS_DROP_FLOW_ALLOW_NAMESPACES` | `-drop-flow-allow-namespaces` | *(empty)* | Comma-separated namespace allow-list for `netobs_drop_events_flow_total` 5-tuple emit (#64). Empty disables emit cluster-wide. RFC1123 validated. See [docs/netobs/drop-flow-analysis.md](docs/netobs/drop-flow-analysis.md) |
| `NETOBS_DROP_FLOW_MAX_ACTIVE` | `-drop-flow-max-active` | `1024` | LRU sampling cap for concurrent active 5-tuple flows in drop flow metric. Older flows are evicted when limit exceeded |
| `NODE_NAME` | `-node-name` | *(hostname)* | Observed Kubernetes node name |
| `KUBE_METADATA_REFRESH` | `-metadata-refresh` | `30s` | Kubernetes informer resync interval |
| `DROP_REASON_FORMAT_PATH` | `-drop-reason-format` | `/sys/kernel/tracing/events/skb/kfree_skb/format` | skb:kfree_skb tracepoint format path. See [docs/netobs/drop-reason.md](docs/netobs/drop-reason.md) for the kernel 6.8 reason code table, the runtime mapping mechanism, the `drop_category` taxonomy, and PromQL examples |

### gpuobs-agent

| Environment Variable | CLI Flag | Default | Description |
|---|---|---|---|
| `LISTEN_ADDR` | `-listen` | `:9820` | HTTP listen address |
| `NODE_NAME` | `-node-name` | *(hostname)* | Observed Kubernetes node name |
| `GPU_POLL_INTERVAL` | `-poll-interval` | `5s` | NVML device polling interval; must be > 0 |
| `GPU_METRICS_ENABLED` | `-gpu-metrics` | `true` | Emit `gpuobs_device_*` metrics; set false to skip device polling entirely |
| `GPUOBS_POD_METRICS_ENABLED` | `-pod-metrics` | `true` | Emit `gpuobs_pod_*` metrics via PID → Pod resolution; disable on large clusters to cap Prometheus cardinality |
| `KUBE_METADATA_REFRESH` | `-metadata-refresh` | `30s` | Kubernetes informer resync interval; must be > 0 |
| `GPUOBS_CUDA_UPROBE_ENABLED` | `-cuda-uprobe` | `true` | Enable libcuda.so uprobe module emitting `gpuobs_cuda_*` counters; requires the host capabilities listed under "Host requirements" below and a libcuda hostPath mount. Pair with `GPUOBS_POD_METRICS_ENABLED=false` to fully suppress per-pod cardinality on large clusters |
| `GPUOBS_CUDA_LIBCUDA_PATH` | `-cuda-libcuda-path` | (empty = auto) | Absolute path to host libcuda.so.1 reachable from inside the container. Empty probes the distro candidate paths (Debian multiarch, RHEL lib64, GPU Operator driver container) under the DaemonSet `/host/usr` and `/host/run/nvidia` mounts (#296) |
| `GPUOBS_CUDA_DEVICEMAP_REFRESH` | `-cuda-devicemap-refresh` | `1s` | Interval between NVML `RunningProcesses` sweeps that rebuild the PID→GPU map and clean up stale `gpuobs_cuda_*` series; must be > 0 |
| `GPUOBS_CUDA_LIBCUDART_PATH` | `-cuda-libcudart-path` | *(empty)* | Absolute path to host `libcudart.so` reachable from inside the container. Empty disables CUDA Runtime API attach. libcudart is part of the CUDA Toolkit (not the driver), so host attach only fires when the workload mmaps the same inode (containers that bundle their own libcudart are not covered, see the limitations note below) |

#### Host requirements

gpuobs-agent runs unprivileged (`privileged: false`, `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`) and relies on a narrow capability set rather than container privilege. The host kernel and Linux capabilities must satisfy the following.

| Requirement | Reason |
|---|---|
| Linux kernel ≥ 6.6 | The cuda uprobe module attaches via the `uprobe_multi` BPF link (`BPF_LINK_CREATE` with `BPF_TRACE_UPROBE_MULTI`), introduced in 6.6. The older `perf_event_open` uprobe PMU path is intentionally avoided because it is blocked by `kernel.perf_event_paranoid` ≥ 2 on hardened distros (Ubuntu defaults to 4) and would otherwise require `CAP_SYS_ADMIN` or sysctl tuning. |
| `CAP_BPF` | Issuing the `bpf()` syscall to load programs, create maps, and create the uprobe_multi link. |
| `CAP_PERFMON` | Loading tracing-class BPF programs (`BPF_PROG_TYPE_KPROBE` covers uprobes). This capability is unrelated to `perf_event_open` paranoid checks in the uprobe_multi path. |
| `CAP_SYS_PTRACE` | Reading `/proc/<pid>/environ` (mode `0400`, cross-uid access guarded by `PTRACE_MODE_READ`) to extract `NVIDIA_VISIBLE_DEVICES` for multi-GPU ordinal-to-UUID mapping when the workload Pod runs as a non-root user. Pod UID extraction from `/proc/<pid>/cgroup` (mode `0444`) does not require this capability but relies on the same `hostPID: true` setting so that NVML-reported PIDs match `/proc` entries. |
| `kernel.perf_event_paranoid` (any value) | Not a constraint after the uprobe_multi switch. The agent has been verified against the Ubuntu default of 4. |
| Host BTF (`/sys/kernel/btf/vmlinux`) readable | CO-RE relocation at load time. Kernel must be built with `CONFIG_DEBUG_INFO_BTF=y`. |

Hosts on kernels older than 6.6 cannot run the cuda uprobe module under this capability set. The simplest mitigation is to disable the module via `GPUOBS_CUDA_UPROBE_ENABLED=false`; device-level NVML metrics and PID-to-Pod resolution continue to work.

### correlation-exporter

correlation-exporter는 두 agent의 Prometheus 메트릭을 입력으로 받아 동작한다. cluster API 권한이 필요 없으며 Prometheus HTTP API만 호출한다. 모든 값은 환경 변수와 CLI flag 양쪽으로 설정 가능하며 flag가 환경 변수보다 우선한다. cluster 배포 시에는 `deploy/correlation/base/configmap.yaml`이 환경 변수 값을 주입한다.

| Environment Variable | CLI Flag | Default | Description |
|---|---|---|---|
| `PROMETHEUS_URL` | `-prometheus-url` | `http://localhost:9090` | Prometheus base URL. cluster 배포 시 base ConfigMap이 `http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090`으로 override |
| `LISTEN_ADDR` | `-listen` | `:9830` | `/metrics`, `/healthz`, `/readyz`를 노출하는 HTTP listen address |
| `RECONCILE_INTERVAL` | `-reconcile-interval` | `5m` | reconcile cycle 간격. dev overlay는 `1m`으로 단축 |
| `WINDOW` | `-window` | `1h` | query_range 윈도우. dev overlay는 `30m`으로 단축 |
| `STEP` | `-step` | `30s` | query_range step. Prometheus scrape interval과 일치시킬 것을 권장 |
| `MIN_SAMPLES` | `-min-samples` | `60` | Pearson 산출 최소 유효 표본 수. NaN / Inf pairwise 제거 후의 개수 |
| `LAG_STEPS` | `-lag-steps` | `-1,0,1` | 산출할 lag step 콤마 리스트. 양수는 suspect가 victim을 앞서는 방향 |
| `TOP_N` | `-top-n` | `10` | victim × dimension 그룹당 채택할 noisy neighbor 수. 상한 100 |
| `FETCH_TIMEOUT` | `-fetch-timeout` | `30s` | 단일 query_range 호출의 HTTP timeout |
| n/a | `-extra-metric` | *(empty)* | DefaultMetrics 외에 추가 fetch할 query. flag 다중 지정 가능 |

## Versioning

The `VERSION` file at the repository root is the single source of truth for every agent image tag. `make bump` increments VERSION with **decimal carry** (`0.1.9` → `0.2.0`, `0.9.9` → `1.0.0`) and rewrites every `deploy/*/overlays/*/kustomization.yaml` image tag it discovers via `find`, so newly added agent overlays are picked up automatically without editing the bump rule.

```bash
make bump    # bump VERSION + update every overlay image tag in one step
```

## Deploy

### Overlay matrix

각 컴포넌트 × 각 rollout stage 의 overlay 들이다. 명령은 `make <action>-<overlay>` 패턴을 따른다.

| Overlay | Component | Stage | Node selector | Image policy |
|---|---|---|---|---|
| `netobs-dev` | netobs | canary | `accelerator=nvidia`, `observability.netobs/canary=true` | `Never` (local image) |
| `netobs-prod` | netobs | fleet | `observability.netobs/enabled=true` (control-plane excluded) | `IfNotPresent` |
| `gpuobs-dev` | gpuobs | canary | `accelerator=nvidia`, `observability.netobs/canary=true` | `Never` (local image) |
| `gpuobs-prod` | gpuobs | fleet | `accelerator=nvidia`, `observability.netobs/enabled=true` | `IfNotPresent` |
| `correlation-dev` | correlation-exporter | cluster singleton | n/a (Deployment, 임의 노드) | `Never` (local image), `RECONCILE_INTERVAL=1m`, `WINDOW=30m` |
| `correlation-prod` | correlation-exporter | cluster singleton | n/a (Deployment, 임의 노드) | `IfNotPresent`, 메모리 limit 1Gi 상향 |

### Node labels

GPU canary node (hosts both `netobs-dev` and `gpuobs-dev`):
```bash
kubectl label node gpu \
  accelerator=nvidia \
  observability.netobs/canary=true \
  observability.netobs/enabled=true \
  --overwrite
```

General worker nodes (targets of `netobs-prod`):
```bash
kubectl label node ebpf-worker1 observability.netobs/enabled=true --overwrite
kubectl label node ebpf-worker2 observability.netobs/enabled=true --overwrite
```

### Dev canary workflow

Replace `<agent>` with `netobs` or `gpuobs`:
```bash
make build-<agent>-agent          # local binary
make image-build-<agent>-agent    # local image at <agent>-agent:<VERSION>
make render-<agent>-dev           # kustomize dry-run
make deploy-<agent>-dev           # apply to canary node
make delete-<agent>-dev           # teardown
```

correlation-exporter는 `-agent` 접미사 컨벤션 밖에 있어 explicit 타깃을 사용한다.
```bash
make build-correlation-exporter        # local binary
make image-build-correlation-exporter  # local image at correlation-exporter:<VERSION>
make render-correlation-dev            # kustomize dry-run
make deploy-correlation-dev            # apply Deployment + ServiceMonitor + PrometheusRule + ConfigMap
make delete-correlation-dev            # teardown
```

### Prod fleet workflow

```bash
make image-build-<agent>-agent    # build image
make image-push-<agent>-agent     # push to ghcr.io/inno-rnd-project/<agent>-agent
make render-<agent>-prod          # kustomize dry-run
make deploy-<agent>-prod          # apply to fleet
make delete-<agent>-prod          # teardown
```

correlation-exporter prod:
```bash
make image-push-correlation-exporter
make deploy-correlation-prod
```

### Umbrella targets

Operate on every agent at once:
```bash
make build-all           # every agent binary
make image-build-all     # every agent image
make image-push-all      # push every agent image
```

## HTTP Endpoints

세 컴포넌트가 같은 endpoint 표면을 노출한다 (netobs: `:9810`, gpuobs: `:9820`, correlation-exporter: `:9830`).

| Path | Description |
|---|---|
| `/metrics` | Prometheus metrics |
| `/healthz` | Liveness probe (항상 200) |
| `/readyz` | Readiness probe. correlation-exporter는 첫 reconcile 성공 전까지 503 |
| `/` | JSON service info (netobs / gpuobs only) |

## Prometheus Metrics

### netobs

| Metric | Type | Labels | Description |
|---|---|---|---|
| `netobs_events_total` | Counter | `stage` | Total eBPF events by stage |
| `netobs_stage_latency_seconds` | Histogram | `stage` | Kernel stage latency |
| `netobs_drop_total` | Counter | `reason` | Drop events by kernel reason code |
| `netobs_stage_events_labeled_total` | Counter | `stage`, `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction`, `dst_namespace`, `dst_workload` | Enriched events by workload |
| `netobs_stage_latency_labeled_seconds` | Histogram | `stage`, `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction`, `dst_namespace`, `dst_workload` | Enriched latency by workload |
| `netobs_drop_events_labeled_total` | Counter | `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction`, `drop_reason`, `drop_category`, `dst_namespace`, `dst_workload` | Enriched drop events with reason |
| `netobs_retrans_events_labeled_total` | Counter | `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction`, `dst_namespace`, `dst_workload` | Enriched retransmission events |
| `netobs_pod_stage_events_labeled_total` | Counter | `stage`, `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `traffic_scope`, `direction`, `dst_namespace`, `dst_workload`, `dst_pod_uid` | Per-pod instance events |
| `netobs_pod_stage_latency_labeled_seconds` | Histogram | `stage`, `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `traffic_scope`, `direction`, `dst_namespace`, `dst_workload`, `dst_pod_uid` | Per-pod instance latency |
| `netobs_dst_classifier_emits_total` | Counter | `outcome` | dst label classification outcome bucket counter (disabled / external / unresolved / service / pod_with_uid / pod_without_uid / other). Use `rate(...{outcome="pod_with_uid"}[5m])` to detect cardinality bombs from misconfigured allow-list namespaces with high pod churn |

> **dst label semantics**: `dst_*` 라벨은 흐름의 반대편 peer를 가리킨다. 외부 IP는 `dst_workload="_external"`, 미해상 흐름은 `dst_workload="_unresolved"`, ClusterIP는 `dst_workload="svc/<name>"`로 마킹된다. `POD_FLOW_DST_ENABLED=false`이면 `dst_namespace`/`dst_workload`/`dst_pod_uid` 셋 다 빈 값으로 emit되어 cardinality가 도입 전 수준으로 collapse된다. `POD_FLOW_DST_UID_ALLOW_NAMESPACES`에 등재되지 않은 namespace의 Pod dst는 `dst_pod_uid`만 빈 값이고 `dst_namespace`/`dst_workload`는 채워진다. 같은 노드 Pod-to-Pod 흐름은 src=A egress와 src=B ingress 두 시리즈로 잡히므로 합산 PromQL은 `direction="egress"`로 필터해 double counting을 회피한다.

> **Cardinality note**: `netobs_pod_stage_*` metrics carry `src_pod`, `src_pod_uid`, and optionally `dst_pod_uid` labels, so each pod redeployment creates a new time series. On large clusters or with frequent pod churn this can inflate Prometheus memory. Set `POD_METRICS_ENABLED=false` (or `-pod-metrics=false`) to opt out of `netobs_pod_*`. Set `POD_FLOW_DST_ENABLED=false` to drop dst labels entirely, or scope `POD_FLOW_DST_UID_ALLOW_NAMESPACES` to specific namespaces only.

#### Stages (netobs)

| Stage | Description |
|---|---|
| `sendmsg_ret` | `tcp_sendmsg` return |
| `to_veth` | Forwarded to veth interface |
| `to_devq` | Forwarded to device queue |
| `retrans` | TCP retransmission |
| `drop` | Packet drop |

### gpuobs

Device-level gauges are sampled from NVML every `GPU_POLL_INTERVAL` (default 5s). Per-pod gauges resolve each running PID via `/proc/<pid>/cgroup` to a Pod UID and join with the Kubernetes informer cache.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `gpuobs_agent_info` | Gauge | `version` | Static agent info, value always 1 |
| `gpuobs_device_utilization_percent` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | GPU compute utilization (0-100) |
| `gpuobs_device_memory_used_bytes` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | GPU memory used (bytes) |
| `gpuobs_device_memory_total_bytes` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | GPU memory total capacity (bytes) |
| `gpuobs_device_temperature_celsius` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | GPU temperature (°C) |
| `gpuobs_device_power_usage_watts` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | GPU power draw (watts) |
| `gpuobs_device_memory_copy_utilization_percent` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | Memory copy engine utilization (0-100) |
| `gpuobs_device_pcie_rx_bytes_per_second` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | PCIe receive rate sampled by NVML (20ms window, normalized to bytes/sec) |
| `gpuobs_device_pcie_tx_bytes_per_second` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | PCIe transmit rate sampled by NVML (20ms window, normalized to bytes/sec) |
| `gpuobs_device_throttle_active` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model`, `reason` | 1 if NVML reports the named throttle reason is currently active; reasons include `gpu_idle`, `sw_power_cap`, `hw_slowdown`, `sw_thermal_slowdown`, `hw_thermal_slowdown`, `hw_power_brake_slowdown`, `applications_clocks_setting`, `sync_boost`, `display_clock_setting` |
| `gpuobs_device_clock_mhz` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model`, `clock` | Current GPU clock frequency in MHz per domain (`clock`=`sm`/`memory`/`graphics`) |
| `gpuobs_device_ecc_errors_total` | Counter | `node`, `gpu_uuid`, `gpu_index`, `gpu_model`, `error_type` | Cumulative ECC error count since the agent started, sourced from NVML VOLATILE counters with delta tracking (`error_type`=`corrected`/`uncorrected`) |
| `gpuobs_device_encoder_utilization_percent` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | NVENC encoder utilization (0-100) |
| `gpuobs_device_decoder_utilization_percent` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | NVDEC decoder utilization (0-100) |
| `gpuobs_device_performance_state` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | NVML performance state (0=highest, 15=idle, 32=unknown) |
| `gpuobs_device_fan_speed_percent` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | GPU fan duty cycle (0-100); absent on passively-cooled cards |
| `gpuobs_device_bar1_memory_used_bytes` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | PCIe BAR1 memory area used (bytes); host-mapped GPU memory |
| `gpuobs_device_bar1_memory_total_bytes` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | PCIe BAR1 memory area capacity (bytes) |
| `gpuobs_device_power_limit_watts` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | Currently configured power management limit (watts) — compare against `gpuobs_device_power_usage_watts` for headroom |
| `gpuobs_device_throttle_violation_seconds_total` | Counter | `node`, `gpu_uuid`, `gpu_index`, `gpu_model`, `reason` | Cumulative throttle violation time (seconds) since the agent started, sourced from NVML `GetViolationStatus` per `PerfPolicyType` with delta tracking and nanosecond-to-second conversion. `reason` ∈ {`power`, `thermal`, `sync_boost`, `board_limit`, `low_utilization`, `reliability`, `total_app_clocks`, `total_base_clocks`}. Complements `gpuobs_device_throttle_active` (instantaneous) with cumulative duration |
| `gpuobs_device_gpm_utilization_percent` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model`, `gpm_metric` | Datacenter-GPU GPM (GPU Performance Monitoring) sampler output (0-100). `gpm_metric` ∈ {`graphics_util`, `sm_occupancy`, `tensor_active`, `dram_bandwidth`}. **Datacenter GPU only** (H100/A100 등); consumer cards (RTX 3090/4090 등) report unsupported and emit no series |
| `gpuobs_device_energy_consumption_joules_total` | Counter | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | Cumulative GPU energy consumption (joules) since the agent started, sourced from NVML `GetTotalEnergyConsumption` with baseline-then-delta tracking and millijoule-to-joule conversion. `rate(...)` 가 곧 J/sec = Watts 평균 |
| `gpuobs_device_pcie_link_generation_current` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | Current PCIe link generation negotiated by the GPU (1-5); idle GPUs may downgrade and recover under load |
| `gpuobs_device_pcie_link_width_current` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | Current PCIe link width negotiated by the GPU (lanes 1-16) |
| `gpuobs_device_pcie_replay_errors_total` | Counter | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | Cumulative PCIe link replay errors with baseline-then-delta tracking. Sustained increase signals riser/cable/slot issues |
| `gpuobs_device_temperature_threshold_celsius` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model`, `threshold` | Static GPU temperature thresholds in Celsius. `threshold` ∈ {`slowdown`, `shutdown`, `mem_max`, `gpu_max`}. Pair with `gpuobs_device_temperature_celsius` for thermal headroom |
| `gpuobs_device_power_limit_enforced_watts` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | Currently enforced GPU power limit (NVML `GetEnforcedPowerLimit`); usually equals `power_limit_watts` but diverges under driver-level capping |
| `gpuobs_device_persistence_mode` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | NVML driver persistence mode (1=enabled, 0=disabled). Disabled mode incurs cold-start cost on first CUDA context creation |
| `gpuobs_device_compute_mode` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model` | NVML compute mode enum (0=Default, 1=ExclusiveThread, 2=Prohibited, 3=ExclusiveProcess) |
| `gpuobs_device_info` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model`, `compute_capability`, `architecture`, `max_pcie_generation`, `max_pcie_width`, `num_cores`, `memory_bus_width_bits` | Static GPU characteristics (value always 1). Use as join target for fleet-wide grouping (e.g., `* on(...) group_left(architecture) gpuobs_device_info`) |
| `gpuobs_device_firmware_info` | Gauge | `node`, `gpu_uuid`, `gpu_index`, `gpu_model`, `vbios_version`, `gsp_firmware_version` | GPU firmware versions (value always 1) for regression debugging |
| `gpuobs_pod_memory_used_bytes` | Gauge | `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `gpu_uuid`, `gpu_index` | GPU memory used (bytes) attributed to a single Pod. Aggregates compute + graphics mode processes, max-by-PID deduplication |
| `gpuobs_cuda_kernel_launches_total` | Counter | `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `gpu_uuid` | Cumulative count of CUDA kernel launches. Sourced from cuLaunchKernel / cuLaunchKernelEx / cuLaunchCooperativeKernel (libcuda.so) and cudaLaunchKernel (libcudart.so when configured); per-Pod via cgroup PID resolution and per-GPU via NVML `RunningProcesses` mapping |
| `gpuobs_cuda_h2d_bytes_total` | Counter | `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `gpu_uuid` | Cumulative bytes copied host→device. Sourced from cuMemcpyHtoD_v2 / cuMemcpyHtoDAsync_v2 (linear), cuMemcpy2D_v2 / cuMemcpy2DAsync_v2 / cuMemcpy3D_v2 / cuMemcpy3DAsync_v2 when srcMemoryType=HOST and dstMemoryType=DEVICE, and cudaMemcpy / cudaMemcpyAsync (libcudart.so when configured) with cudaMemcpyKind=HostToDevice. Total bytes for 2D/3D variants are computed as WidthInBytes × Height × Depth |
| `gpuobs_cuda_d2h_bytes_total` | Counter | `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `gpu_uuid` | Cumulative bytes copied device→host. Sourced from cuMemcpyDtoH_v2 / cuMemcpyDtoHAsync_v2, the cuMemcpy2D_v2 / 3D_v2 family with srcMemoryType=DEVICE and dstMemoryType=HOST, and cudaMemcpy / cudaMemcpyAsync with cudaMemcpyKind=DeviceToHost |
| `gpuobs_cuda_dtod_bytes_total` | Counter | `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `gpu_uuid` | Cumulative bytes copied device→device. Sourced from cuMemcpyDtoD_v2 / cuMemcpyDtoDAsync_v2, the cuMemcpy2D_v2 / 3D_v2 family with both srcMemoryType=DEVICE and dstMemoryType=DEVICE, and cudaMemcpy / cudaMemcpyAsync with cudaMemcpyKind=DeviceToDevice |
| `gpuobs_cuda_unknown_dir_bytes_total` | Counter | `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `gpu_uuid` | Cumulative bytes copied via memcpy paths whose direction the BPF layer cannot determine. Includes cuMemcpy (UVA, both ptrs are CUdeviceptr), cuMemcpy2D / 3D variants whose srcMemoryType / dstMemoryType is ARRAY, UNIFIED, or HOST→HOST, and cudaMemcpy / cudaMemcpyAsync with cudaMemcpyKind=Default (UVA) or HostToHost. Sustained nonzero rate indicates the workload uses UVA or unified memory and warrants further analysis with cuPointerGetAttribute outside this agent |
| `gpuobs_cuda_symbol_available` | Gauge | `node`, `symbol` | 1 if the named libcuda.so symbol was successfully resolved and uprobe-attached at agent startup, 0 otherwise. Every tracked symbol is pre-registered as 0 before attach is attempted, so a missing series indicates the agent itself never started, distinguishing libcuda-load failures from per-symbol attach failures |
| `gpuobs_cuda_events_lost_total` | Counter | `node` | Cumulative count of CUDA uprobe events dropped because `bpf_ringbuf_reserve` returned NULL (ring buffer full). The count is maintained in a BPF percpu counter and polled every `GPUOBS_CUDA_DEVICEMAP_REFRESH`; sustained `rate(...)` > 0 indicates the ring buffer (default 4 MiB) is undersized for the current event volume |
| `gpuobs_cuda_pid_multi_gpu_count` | Gauge | `node` | Number of host PIDs observed by NVML `RunningProcesses` on more than one GPU during the most recent device-map refresh. DDP-style workloads with one process per GPU keep this at 0; single-process multi-GPU usage (`nn.DataParallel`, model parallelism, custom multi-context CUDA) drives it above 0. When non-zero, per-event GPU attribution depends on cudaSetDevice / cuCtxSetCurrent uprobe coverage (see the multi-GPU pod note below) |

> **Cardinality note**: `gpuobs_pod_memory_used_bytes` and `gpuobs_cuda_*` counters carry `src_pod` and `src_pod_uid` labels, mirroring `netobs_pod_stage_*` so the four shared keys (`node`, `src_namespace`, `src_pod`, `src_pod_uid`) join cleanly in PromQL. On large clusters or with frequent pod churn this can inflate Prometheus memory. Set `GPUOBS_POD_METRICS_ENABLED=false` (or `-pod-metrics=false`) to opt out of `gpuobs_pod_*`, and pair with `GPUOBS_CUDA_UPROBE_ENABLED=false` (or `-cuda-uprobe=false`) to also stop emitting `gpuobs_cuda_*`.

On NVML initialization failure (non-GPU node, driver missing) or when `GPU_METRICS_ENABLED=false`, the collector logs a warning and skips device polling; `gpuobs_device_*` series are not emitted, and `/healthz`·`/readyz` continue to return 200. When the kube informer cache has not synced, `/readyz` reports `kube resolver informer not synced` until the initial sync completes.

> **Per-metric NVML support detection**: NVML returns `NVML_ERROR_NOT_SUPPORTED` for metrics that the GPU model does not expose (e.g., ECC counters on consumer cards, encoder util on some SKUs). gpuobs detects this on the first call per metric per device, logs a single warning, then silently skips that metric in subsequent polls — only the supported metric series are emitted, keeping Prometheus cardinality minimal.

> **GPM hardware requirement**: `gpuobs_device_gpm_utilization_percent` requires a GPM-capable GPU (H100/H200, A100/A30 등 데이터센터 카드). On consumer cards the `nvmlGpmQueryDeviceSupport` call returns unsupported and the agent skips GPM sampling entirely — no series are created. GPM also needs two consecutive samples to compute the first metric, so the very first poll after agent start emits no GPM series even on supported hardware.

> **Why no `gpuobs_pod_sm_utilization_percent`**: NVML's `nvmlDeviceGetProcessUtilization` exposes a 6-second sliding window sampler, which is too coarse for short-lived training steps and can miss bursts entirely. Per-pod compute utilization is deferred until a more precise data source is available; only memory attribution is published in Phase 3.

> **hostPID requirement**: NVML returns host-namespace PIDs, so the gpuobs DaemonSet sets `hostPID: true` to read `/proc/<pid>/cgroup` for Pod UID extraction.

> **CUDA uprobe environment requirements**: The `gpuobs_cuda_*` counters are produced by attaching user-space probes to libcuda.so via the kernel `perf_uprobe` PMU. This requires (a) `privileged: true` on the agent container; `CAP_BPF`/`CAP_PERFMON`/`CAP_SYS_PTRACE` alone are blocked by typical `kernel.perf_event_paranoid` settings, and (b) a hostPath mount of the host's library tree (`/usr` → `/host/usr`, plus `/run/nvidia` → `/host/run/nvidia` for GPU Operator driver containers) so that the probe is registered against the same inode that other processes mmap. `GPUOBS_CUDA_LIBCUDA_PATH` pins the path explicitly; when empty the agent probes the distro candidate paths (#296).

> **CUDA Runtime API (libcudart) attach**: `cudaLaunchKernel` / `cudaMemcpy` / `cudaMemcpyAsync` are tracked when `GPUOBS_CUDA_LIBCUDART_PATH` is set to a host-side libcudart absolute path that is bind-mounted into the agent container. Empty value (default) disables CUDA Runtime API attach and the corresponding `gpuobs_cuda_symbol_available` series remain at 0. libcudart is part of the CUDA Toolkit (not the driver), so its host inode is shared only with workloads that mmap the same file. Containers that bundle their own libcudart (PyTorch / TensorFlow images that ship `/usr/local/cuda/.../libcudart.so.X.Y`) do not share the host inode, so host-attach uprobes do not fire for them; per-pod inode tracking for that case is a follow-up. Operators who run workloads that link the host libcudart directly (e.g. baremetal CUDA samples or custom binaries) configure the path and add a corresponding hostPath mount via a kustomize patch.

> **CUDA uprobe short-lived workload limit**: Because `gpu_uuid` attribution is built from NVML `RunningProcesses` polled every `GPUOBS_CUDA_DEVICEMAP_REFRESH` (default 1s), a CUDA process that lives for less than one polling interval will not appear in any active key set. Its first events land with `gpu_uuid="unknown"` until the next refresh, and if the process exits before that refresh those series are deleted on the very next cleanup cycle. PyTorch/TensorFlow long-running training/inference workloads are unaffected; one-shot CLI binaries (e.g., a single `vectorAdd` invocation) may emit only "unknown"-labeled samples.

> **CUDA uprobe multi-GPU pod**: A Pod that drives more than one GPU on the same node is detected by NVML `RunningProcesses` listing the same PID under multiple GPUs, and the operator gauge `gpuobs_cuda_pid_multi_gpu_count` reflects that count every refresh cycle. DDP-style workloads with one process per GPU keep this at 0; single-process multi-GPU usage (`nn.DataParallel`, model parallelism, custom multi-context CUDA) drives it above 0. For multi-GPU PIDs the agent attributes each kernel launch / memcpy to its specific GPU by hooking `cudaSetDevice` (Runtime API) and `cuCtxCreate_v2` + `cuCtxSetCurrent` (Driver API) so the BPF layer captures the per-thread current device ordinal at event time, then translates it to a host GPU UUID via the `NVIDIA_VISIBLE_DEVICES` environment variable read from `/proc/<pid>/environ`. When the BPF map has no entry for the firing thread (e.g., `cuCtxCreate_v3` or `cuDevicePrimaryCtxRetain`-created contexts that the agent does not hook, or programs that never call `cudaSetDevice`) the agent falls back to the legacy PID-level NVML mapping — which remains last-GPU-wins for multi-GPU PIDs. `cuCtxPushCurrent` / `cuCtxPopCurrent` per-thread context stacks are not yet tracked; programs that rely on them for active multi-context push/pop within a single thread fall back to the most recent `cuCtxSetCurrent` value.

> **CUDA uprobe NVML dependency**: When `GPUOBS_CUDA_UPROBE_ENABLED=true` but NVML is unavailable (driver missing, `GPU_METRICS_ENABLED=false`, etc.), the cuda reader is **not** started. NVML is the source of both the PID→GPU map and the periodic `RetainCudaSeries` cleanup signal — without it, terminated-Pod series and the in-process key set would accumulate without bound. Operators see a `cuda uprobe enabled but NVML is unavailable` warn log; `gpuobs_cuda_*` series are not emitted on such nodes.

> **GPU device hot-plug**: Both the device-level collector and the CUDA uprobe reader rebuild their device handle set every NVML poll via `nvml.DeviceSet.Sync`, which keys on GPU UUID rather than NVML index. Newly added GPUs are picked up on the next sync and removed GPUs have their handles closed automatically; index reordering after a hot-remove is absorbed without re-attaching probes for surviving devices. GPU device reset / driver reload that re-introduces the same UUID is out of scope and is tracked separately.

### correlation-exporter

correlation-exporter는 두 agent의 cause score recording rule과 latency 메트릭을 입력으로 받아 Pearson 상관계수를 산출한 후 Top-N noisy neighbor 페어를 emit한다. 산출 알고리즘 6단계, lag 의미, dimension 분류 우선순위, 가드 / fallback 5종은 [`internal/correlation/README.md`](internal/correlation/README.md) 의 산출 알고리즘 절에 정리되어 있다.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `correlation_noisy_neighbor_score` | Gauge | `victim_namespace`, `victim_pod`, `victim_pod_uid`, `suspect_namespace`, `suspect_pod`, `suspect_pod_uid`, `resource_dimension`, `rank` | Pearson 상관계수 최대 절대값 (0-1). suspect 자원 압박과 victim latency 동조 강도. `resource_dimension` ∈ {`cpu`, `memory`, `network`, `gpu`}, `rank`는 1부터 `TOP_N` (기본 10) 까지 |
| `correlation_noisy_neighbor_lag_seconds` | Gauge | (동일) | score가 최대 절대값을 보인 lag의 초 단위 환산. 양수면 suspect 변동이 victim latency를 N초 선행하는 인과 방향 |
| `correlation_reconcile_duration_seconds` | Gauge | n/a | 마지막 reconcile cycle의 walltime |
| `correlation_reconcile_pairs_total` | Counter | n/a | `Correlate` 산출 페어의 누적 합계 |
| `correlation_reconcile_neighbors_total` | Counter | n/a | `SelectTopN` 채택 후 emit된 noisy neighbor 엔트리의 누적 합계 |
| `correlation_reconcile_skipped_total` | Counter | `reason` | skip된 페어의 누적 합계. `reason` ∈ {`low_samples`, `constant`} |
| `correlation_reconcile_partial_total` | Counter | n/a | 일부 query가 데이터를 만들지 못한 cycle의 누적. `metrics_observed < metrics_expected` 인 cycle마다 증가 |
| `correlation_reconcile_metrics_expected` | Gauge | n/a | 마지막 cycle의 DefaultMetrics + ExtraMetrics 총 query 수 |
| `correlation_reconcile_metrics_observed` | Gauge | n/a | 마지막 cycle의 결과에 등장한 distinct src/dst metric 수 |
| `correlation_reconcile_last_success_timestamp_seconds` | Gauge | n/a | 마지막 성공 reconcile의 epoch 초. `CorrelationExporterStalled` alert의 입력 |
| `correlation_reconcile_errors_total` | Counter | n/a | reconcile cycle이 wrapped error로 종료된 횟수의 누적 합계 |

> **카디널리티**: 각 noisy neighbor 엔트리는 동일 라벨 셋으로 `correlation_noisy_neighbor_score`와 `correlation_noisy_neighbor_lag_seconds` 두 gauge를 함께 emit하므로 victim 1000 pod × dimension 4 × rank 10 × metric 2 = 80,000 series가 상한이다. self-health 9종을 더해도 80,009 series. `TOP_N`의 binary-side 상한 100으로 운영자의 입력 실수에 의한 series 폭주를 차단한다.

> **PrometheusRule alerts**: `deploy/correlation/base/prometheus-rule.yaml`이 세 alert를 정의한다. `CorrelationStrongNoisyNeighbor` (max_abs_value > 0.85가 10분 지속), `CorrelationExporterStalled` (last success가 10분 이상 정체), `CorrelationExporterReconcileErrors` (10분 윈도우 에러 3회 이상). 각 alert의 `runbook_url` annotation이 `internal/correlation/README.md`의 헤딩 anchor와 일치해 운영자가 즉시 대응 절차로 이동 가능하다.

## Performance & overhead measurement

The CUDA uprobe dispatch hot path includes a PID-to-PodIdentity cache (introduced in v0.3.5) that absorbs the per-event `/proc/<pid>/cgroup` parse cost; without the cache, a single PyTorch ResNet50 inference loop drove the agent container to ~740 mCPU on a 27 K Hz kernel launch rate, which dropped to ~184 mCPU after the cache landed (75% reduction). The measurement methodology, PromQL queries, decision gate, and full results across baseline / uprobe-enabled / cache-enabled snapshots are in [`docs/perf/cuda-uprobe-overhead.md`](docs/perf/cuda-uprobe-overhead.md). Reproducible workload manifests (PyTorch ResNet50, Conv2d-only, cuda-sample vectorAdd) live under [`test/perf/`](test/perf/) for any follow-up overhead measurement on a different cluster or workload mix.

## Dashboards

`deploy/dashboards/` 에 5 개의 Grafana dashboard 가 ConfigMap 으로 배포된다. Grafana sidecar 가 `grafana_dashboard=1` label 로 자동 발견한다. uid 와 역할은 다음과 같다.

| Dashboard | uid | 역할 |
|---|---|---|
| observability overview | `observability-overview` | cluster → node → pod drill-down 의 entry point. 4 카테고리 health single-stat, alert annotation, noisy neighbor Top-N 통합 |
| netobs | `netobs-overview` | 네트워크 상세 (drop reason, retrans, stage latency, dst classifier) |
| gpuobs | `gpuobs-overview` | GPU 상세 (device utilization, throttle reason, GPM, NVML 메트릭 카탈로그) |
| correlation noisy neighbor | `correlation-overview` | `correlation-exporter` (#51) 산출의 Top-N noisy neighbor 분석 |
| workload injector | `workload-injector` | `workload-injector` (#52) 합성 부하 timeline 과 blast radius heatmap |

운영자는 평상시 `observability overview` 에서 cluster 전체 상태를 보다가 이상 신호가 잡히면 노드 / Pod 단계 drill-down 후 필요시 4 개의 detailed view 로 이동한다. 워크플로 상세는 [docs/observability/dashboard-workflow.md](docs/observability/dashboard-workflow.md) 에 정리되어 있다.

## Observability — netobs/gpuobs Correlation

netobs와 gpuobs는 동일한 4개 라벨 키 (`node`, `src_namespace`, `src_pod`, `src_pod_uid`) 를 노출해 PromQL `* on(node, src_namespace, src_pod, src_pod_uid) group_left(...)` 패턴으로 join 가능하다. 이 절은 양쪽 메트릭 라벨 일치성 표, recording rule 카탈로그, dashboard 작성 시 즉시 활용 가능한 PromQL 예제 9종을 정의한다. 본 절의 recording rule들은 동시에 correlation-exporter의 `DefaultMetrics` 입력으로 reuse된다.

### Pod-level 라벨 일치성 (4-key join)

| Metric | 공통 join 키 | 추가 라벨 |
|---|---|---|
| `netobs_pod_stage_events_labeled_total` | `node`, `src_namespace`, `src_pod`, `src_pod_uid` | `stage`, `traffic_scope`, `direction` |
| `netobs_pod_stage_latency_labeled_seconds` | `node`, `src_namespace`, `src_pod`, `src_pod_uid` | `stage`, `traffic_scope`, `direction` |
| `gpuobs_pod_memory_used_bytes` | `node`, `src_namespace`, `src_pod`, `src_pod_uid` | `gpu_uuid`, `gpu_index` |

`netobs_pod_stage_events_total` / `netobs_pod_stage_latency_seconds` 등 workload-level (`src_workload`) 메트릭은 pod-instance 키 셋이 다르므로 본 join 대상이 아니다. gpuobs `_device_*` 메트릭(`gpu_uuid` / `gpu_index` 만 가짐) 은 노드 / GPU 단위 분석용으로 별도 group 처리한다.

### Recording rules (`deploy/gpuobs/base/prometheus-rule-*.yaml`)

PrometheusRule CR `netobs-gpuobs-correlation` 에 group `netobs-gpuobs.recording` (interval 30s) 으로 배포한다. agent base kustomization에 포함되어 `make deploy-gpuobs-{dev,prod}` 시 함께 배포된다.

| Record | 단위 | 표현식 골자 |
|---|---|---|
| `node:gpu_util_p95:5m` | 0-100 | `max by(node) (quantile_over_time(0.95, gpuobs_device_utilization_percent[5m]))` |
| `pod:gpu_memory_used_avg:5m` | bytes | `avg_over_time(gpuobs_pod_memory_used_bytes[5m])` |
| `pod:network_egress_rate:5m` | events/sec | `sum by(node, src_namespace, src_pod, src_pod_uid) (rate(netobs_pod_stage_events_labeled_total{direction="egress"}[5m]))` |
| `node:gpu_throttle_seconds:rate5m` | seconds/sec | `sum by(node, reason) (rate(gpuobs_device_throttle_violation_seconds_total[5m]))` |
| `node:gpu_power_headroom_watts` | watts | `min by(node) (gpuobs_device_power_limit_watts - gpuobs_device_power_usage_watts)` — 노드 내 가장 위험한 GPU 기준 |
| `pod:gpu_memory_utilization_ratio:5m` | 0-1 | `avg_over_time(gpuobs_pod_memory_used_bytes[5m]) / on(node, gpu_uuid, gpu_index) group_left() gpuobs_device_memory_total_bytes` |
| `node:gpu_ecc_errors:rate5m` | errors/sec | `sum by(node, error_type) (rate(gpuobs_device_ecc_errors_total[5m]))` |
| `node:gpu_energy_watts_avg:5m` | watts | `sum by(node) (rate(gpuobs_device_energy_consumption_joules_total[5m]))` — J/sec = W 단위로 5분 평균 전력 |
| `node:gpu_pcie_replay:rate5m` | errors/sec | `sum by(node) (rate(gpuobs_device_pcie_replay_errors_total[5m]))` — PCIe 헬스 |
| `node:gpu_temperature_headroom_celsius` | celsius | `min by(node, threshold) (gpuobs_device_temperature_threshold_celsius - ignoring(threshold) group_left() gpuobs_device_temperature_celsius)` — threshold별 thermal 안전 마진. 노드 내 가장 위험한 GPU 기준으로 (node, threshold) 당 1 시리즈 |

> **빈 시리즈 정상 케이스**: `pod:gpu_memory_used_avg:5m` / `pod:gpu_memory_utilization_ratio:5m` 는 GPU를 사용하는 Pod이 없으면 base 시리즈가 부재해 빈 결과를 산출한다. `node:gpu_ecc_errors:rate5m` 은 컨슈머 GPU(RTX 3090 등) 에서 ECC 미지원으로 base 시리즈 자체가 발행되지 않아 빈 결과가 정상 동작이다.

> **rule 명명 규칙 — `pod:network_egress_rate:5m` 단위**: netobs는 byte 카운터를 노출하지 않으므로 본 record 의 단위는 events/sec(이벤트 빈도) 이며 byte rate가 아님. dashboard에서 "egress 트래픽 강도" 추세 지표로 활용한다.

### PromQL 예제

#### 1. 노드별 GPU 평균 사용률
```promql
avg by(node) (gpuobs_device_utilization_percent)
```

#### 2. throttle reason 분해 (현재 활성 reason 표시)
```promql
sum by(reason) (gpuobs_device_throttle_active)
```

#### 3. clock 도메인별 평균 frequency
```promql
avg by(clock) (gpuobs_device_clock_mhz)
```

#### 4. Pod별 GPU 메모리 + 네트워크 활동 join (4-key)
```promql
gpuobs_pod_memory_used_bytes
  * on(node, src_namespace, src_pod, src_pod_uid) group_left()
  sum by(node, src_namespace, src_pod, src_pod_uid) (
    rate(netobs_pod_stage_events_labeled_total[5m])
  )
```

#### 5. Pod 네트워크 latency p95와 GPU 메모리 동시 표기
```promql
# 1) latency p95
histogram_quantile(0.95,
  sum by(le, node, src_namespace, src_pod, src_pod_uid) (
    rate(netobs_pod_stage_latency_labeled_seconds_bucket[5m])
  )
)
# 2) 같은 키로 GPU 메모리 join — group_left로 pod 라벨 보존
* on(node, src_namespace, src_pod, src_pod_uid) group_left()
  gpuobs_pod_memory_used_bytes
```

#### 6. GPM 라벨 분해 (데이터센터 GPU 한정)
```promql
# 데이터센터 GPU 노드에서만 시리즈 산출. RTX 3090 등 컨슈머 카드는 빈 결과.
gpuobs_device_gpm_utilization_percent{gpm_metric="sm_occupancy"}
```

#### 7. Architecture별 평균 전력 (info gauge join)
```promql
# device_info 의 architecture 라벨을 group_left로 끌어와 GPU 세대별 전력 추세 비교.
sum by(architecture) (
  rate(gpuobs_device_energy_consumption_joules_total[5m])
  * on(node, gpu_uuid, gpu_index) group_left(architecture)
    gpuobs_device_info
)
```

#### 8. PCIe 링크 다운그레이드 감지
```promql
# 현재 gen이 max gen보다 낮으면 다운그레이드 상태. info gauge 의 max_pcie_generation 을 라벨에서 직접 비교할 수는 없어
# 운영자가 dashboard에서 단순 gauge 시각화 후 max_pcie_generation 라벨을 별도 패널로 함께 표기하는 방식이 자연스럽다.
gpuobs_device_pcie_link_generation_current
```

#### 9. Thermal headroom 알림 후보 쿼리
```promql
# 어떤 GPU가 slowdown threshold까지 5도 이내인지 즉시 식별.
node:gpu_temperature_headroom_celsius{threshold="slowdown"} < 5
```

### 검증 방법

dev 배포 후 PrometheusRule 등록 + 시리즈 산출을 다음 절차로 확인한다.

```bash
# 1) PrometheusRule CR 등록 확인
kubectl -n ebpf-project get prometheusrules

# 2) Prometheus operator pickup 확인 (rule 파일이 mount되었는지)
PROM_POD=$(kubectl -n monitoring get pod -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
kubectl -n monitoring exec $PROM_POD -c prometheus -- \
  ls /etc/prometheus/rules/prometheus-kube-prometheus-stack-prometheus-rulefiles-0/ | grep correlation

# 3) 각 record가 산출하는 시리즈 수 확인 (rule 10종 전체)
for q in 'node:gpu_util_p95:5m' 'pod:gpu_memory_used_avg:5m' 'pod:network_egress_rate:5m' \
         'node:gpu_throttle_seconds:rate5m' 'node:gpu_power_headroom_watts' \
         'pod:gpu_memory_utilization_ratio:5m' 'node:gpu_ecc_errors:rate5m' \
         'node:gpu_energy_watts_avg:5m' 'node:gpu_pcie_replay:rate5m' \
         'node:gpu_temperature_headroom_celsius'; do
  kubectl -n monitoring exec $PROM_POD -c prometheus -- \
    wget -qO- "http://localhost:9090/api/v1/query?query=${q}"
done
```

5분 윈도우 record는 평가 시작 5분 후부터 의미 있는 결과를 내므로 검증은 충분한 대기 후 수행한다.

## Notes

- If `bpf/netlat.bpf.c` changes, regenerate the embedded BPF artifacts first:
  ```bash
  make generate
  ```
