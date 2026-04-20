# observability-agent

Custom eBPF-based observability agent for Kubernetes GPU nodes.
Currently supports network latency tracing; additional observation targets will be added in future releases.

## Prerequisites

- Go 1.22+
- clang (BPF compilation)
- bpftool (vmlinux.h generation)
- Linux kernel with BTF support

## Local build
```bash
make deps
make build
```

## Local run
```bash
sudo ./bin/netobs-agent -listen :9810 -print-events=true
```

## Configuration

| Environment Variable | CLI Flag | Default | Description |
|---|---|---|---|
| `TARGET_IP` | `-target-ip` | *(empty, trace all)* | Target Pod IPv4 to trace |
| `LISTEN_ADDR` | `-listen` | `:9810` | HTTP listen address |
| `PRINT_EVENTS` | `-print-events` | `true` | Print events to stdout |
| `POD_METRICS_ENABLED` | `-pod-metrics` | `true` | Emit per-pod-instance metrics (`netobs_pod_stage_*`); disable on large clusters to cap Prometheus cardinality |
| `NODE_NAME` | `-node-name` | *(hostname)* | Observed Kubernetes node name |
| `KUBE_METADATA_REFRESH` | `-metadata-refresh` | `30s` | Kubernetes informer resync interval |
| `DROP_REASON_FORMAT_PATH` | `-drop-reason-format` | `/sys/kernel/tracing/events/skb/kfree_skb/format` | skb:kfree_skb tracepoint format path |

## Versioning

Image version is managed via the `VERSION` file at the project root.
`make bump` increments the patch version and updates both overlay manifests.

```bash
make bump          # 0.1.0 → 0.1.1 (VERSION + kustomization.yaml)
make image-build   # build local image (netobs-agent:0.1.1)
make deploy-dev    # deploy to GPU canary node with local image
make image-push    # tag & push to ghcr.io/inno-rnd-project/netobs-agent:0.1.1
make deploy-prod   # deploy to all nodes with registry image
```

## Deploy

### Overlay roles

| Overlay | Purpose | Node selector | Image policy |
|---|---|---|---|
| `dev` | GPU canary | `accelerator=nvidia`, `observability.netobs/canary=true` | `Never` (local) |
| `prod` | Full rollout | `observability.netobs/enabled=true` (control-plane excluded) | `IfNotPresent` |

### Node labels

GPU canary node:
```bash
kubectl label node gpu accelerator=nvidia --overwrite
kubectl label node gpu observability.netobs/canary=true --overwrite
kubectl label node gpu observability.netobs/enabled=true --overwrite
```

General worker nodes:
```bash
kubectl label node ebpf-worker1 observability.netobs/enabled=true --overwrite
kubectl label node ebpf-worker2 observability.netobs/enabled=true --overwrite
```

### Deploy / Delete

```bash
# render manifests (dry-run)
make render-dev
make render-prod

# apply
make deploy-dev
make deploy-prod

# teardown
make delete-dev
make delete-prod
```

## HTTP Endpoints

| Path | Description |
|---|---|
| `/metrics` | Prometheus metrics |
| `/healthz` | Liveness probe |
| `/readyz` | Readiness probe |
| `/` | JSON service info |

## Prometheus Metrics

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

### Stages

| Stage | Description |
|---|---|
| `sendmsg_ret` | `tcp_sendmsg` return |
| `to_veth` | Forwarded to veth interface |
| `to_devq` | Forwarded to device queue |
| `retrans` | TCP retransmission |
| `drop` | Packet drop |

## Notes
- If `bpf/netlat.bpf.c` changes, regenerate embedded BPF artifacts first:
```bash
make generate
```
