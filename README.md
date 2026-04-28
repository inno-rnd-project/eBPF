# observability-agent

Kubernetes observability agent suite combining eBPF-based network latency tracing (`netobs`) and NVML-based GPU state collection (`gpuobs`). Both agents are built and deployed from a single repository with symmetric structure and each runs as an independent DaemonSet.

## Prerequisites

Shared:
- Go 1.22+
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
make build-netobs-agent     # netobs-agent binary (runs BPF regeneration first)
make build-gpuobs-agent     # gpuobs-agent binary
make build-all              # both agents
```

## Local run

```bash
# netobs needs root for BPF loading
sudo ./bin/netobs-agent -listen :9810 -print-events=true

# gpuobs does not need root
./bin/gpuobs-agent -listen :9820
```

## Configuration

### netobs-agent

| Environment Variable | CLI Flag | Default | Description |
|---|---|---|---|
| `TARGET_IP` | `-target-ip` | *(empty, trace all)* | Target Pod IPv4 to trace |
| `LISTEN_ADDR` | `-listen` | `:9810` | HTTP listen address |
| `PRINT_EVENTS` | `-print-events` | `false` | Print events to stdout |
| `POD_METRICS_ENABLED` | `-pod-metrics` | `true` | Emit per-pod-instance metrics (`netobs_pod_stage_*`); disable on large clusters to cap Prometheus cardinality |
| `NODE_NAME` | `-node-name` | *(hostname)* | Observed Kubernetes node name |
| `KUBE_METADATA_REFRESH` | `-metadata-refresh` | `30s` | Kubernetes informer resync interval |
| `DROP_REASON_FORMAT_PATH` | `-drop-reason-format` | `/sys/kernel/tracing/events/skb/kfree_skb/format` | skb:kfree_skb tracepoint format path |

### gpuobs-agent

| Environment Variable | CLI Flag | Default | Description |
|---|---|---|---|
| `LISTEN_ADDR` | `-listen` | `:9820` | HTTP listen address |
| `NODE_NAME` | `-node-name` | *(hostname)* | Observed Kubernetes node name |
| `GPU_POLL_INTERVAL` | `-poll-interval` | `5s` | NVML device polling interval; must be > 0 |
| `GPU_METRICS_ENABLED` | `-gpu-metrics` | `true` | Emit `gpuobs_device_*` metrics; set false to skip device polling entirely |
| `GPUOBS_POD_METRICS_ENABLED` | `-pod-metrics` | `true` | Emit `gpuobs_pod_*` metrics via PID → Pod resolution; disable on large clusters to cap Prometheus cardinality |
| `KUBE_METADATA_REFRESH` | `-metadata-refresh` | `30s` | Kubernetes informer resync interval; must be > 0 |

## Versioning

The `VERSION` file at the repository root is the single source of truth for every agent image tag. `make bump` increments VERSION with **decimal carry** (`0.1.9` → `0.2.0`, `0.9.9` → `1.0.0`) and rewrites every `deploy/*/overlays/*/kustomization.yaml` image tag it discovers via `find`, so newly added agent overlays are picked up automatically without editing the bump rule.

```bash
make bump    # bump VERSION + update every overlay image tag in one step
```

## Deploy

### Overlay matrix

Each agent × each rollout stage gives four overlays. Commands follow the `make <action>-<agent>-<stage>` pattern.

| Overlay | Agent | Stage | Node selector | Image policy |
|---|---|---|---|---|
| `netobs-dev` | netobs | canary | `accelerator=nvidia`, `observability.netobs/canary=true` | `Never` (local image) |
| `netobs-prod` | netobs | fleet | `observability.netobs/enabled=true` (control-plane excluded) | `IfNotPresent` |
| `gpuobs-dev` | gpuobs | canary | `accelerator=nvidia`, `observability.netobs/canary=true` | `Never` (local image) |
| `gpuobs-prod` | gpuobs | fleet | `accelerator=nvidia`, `observability.netobs/enabled=true` | `IfNotPresent` |

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

### Prod fleet workflow

```bash
make image-build-<agent>-agent    # build image
make image-push-<agent>-agent     # push to ghcr.io/inno-rnd-project/<agent>-agent
make render-<agent>-prod          # kustomize dry-run
make deploy-<agent>-prod          # apply to fleet
make delete-<agent>-prod          # teardown
```

### Umbrella targets

Operate on every agent at once:
```bash
make build-all           # every agent binary
make image-build-all     # every agent image
make image-push-all      # push every agent image
```

## HTTP Endpoints

Both agents expose the same endpoints (netobs: `:9810`, gpuobs: `:9820`).

| Path | Description |
|---|---|
| `/metrics` | Prometheus metrics |
| `/healthz` | Liveness probe |
| `/readyz` | Readiness probe |
| `/` | JSON service info (includes agent name) |

## Prometheus Metrics

### netobs

| Metric | Type | Labels | Description |
|---|---|---|---|
| `netobs_events_total` | Counter | `stage` | Total eBPF events by stage |
| `netobs_stage_latency_seconds` | Histogram | `stage` | Kernel stage latency |
| `netobs_drop_total` | Counter | `reason` | Drop events by kernel reason code |
| `netobs_stage_events_labeled_total` | Counter | `stage`, `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction` | Enriched events by workload |
| `netobs_stage_latency_labeled_seconds` | Histogram | `stage`, `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction` | Enriched latency by workload |
| `netobs_drop_events_labeled_total` | Counter | `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction`, `drop_reason`, `drop_category` | Enriched drop events with reason |
| `netobs_retrans_events_labeled_total` | Counter | `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction` | Enriched retransmission events |
| `netobs_pod_stage_events_labeled_total` | Counter | `stage`, `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `traffic_scope`, `direction` | Per-pod instance events |
| `netobs_pod_stage_latency_labeled_seconds` | Histogram | `stage`, `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `traffic_scope`, `direction` | Per-pod instance latency |

> **Cardinality note**: `netobs_pod_stage_*` metrics carry `src_pod` and `src_pod_uid` labels, so each pod redeployment creates a new time series. On large clusters or with frequent pod churn this can inflate Prometheus memory. Set `POD_METRICS_ENABLED=false` (or `-pod-metrics=false`) to opt out.

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
| `gpuobs_pod_memory_used_bytes` | Gauge | `node`, `src_namespace`, `src_pod`, `src_pod_uid`, `gpu_uuid`, `gpu_index` | GPU memory used (bytes) attributed to a single Pod |

> **Cardinality note**: `gpuobs_pod_memory_used_bytes` carries `src_pod` and `src_pod_uid` labels, mirroring `netobs_pod_stage_*` so the four shared keys (`node`, `src_namespace`, `src_pod`, `src_pod_uid`) join cleanly in PromQL. On large clusters or with frequent pod churn this can inflate Prometheus memory. Set `GPUOBS_POD_METRICS_ENABLED=false` (or `-pod-metrics=false`) to opt out.

On NVML initialization failure (non-GPU node, driver missing) or when `GPU_METRICS_ENABLED=false`, the collector logs a warning and skips device polling; `gpuobs_device_*` series are not emitted, and `/healthz`·`/readyz` continue to return 200. When the kube informer cache has not synced, `/readyz` reports `kube resolver informer not synced` until the initial sync completes.

> **Why no `gpuobs_pod_sm_utilization_percent`**: NVML's `nvmlDeviceGetProcessUtilization` exposes a 6-second sliding window sampler, which is too coarse for short-lived training steps and can miss bursts entirely. Per-pod compute utilization is deferred until a more precise data source is available; only memory attribution is published in Phase 3.

> **hostPID requirement**: NVML returns host-namespace PIDs, so the gpuobs DaemonSet sets `hostPID: true` to read `/proc/<pid>/cgroup` for Pod UID extraction. The container remains non-privileged with `capabilities.drop: ALL`; only read access to procfs is gained.

## Notes

- If `bpf/netlat.bpf.c` changes, regenerate the embedded BPF artifacts first:
  ```bash
  make generate
  ```
