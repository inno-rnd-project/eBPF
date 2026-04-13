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

## Build image
```bash
make image-build IMAGE=netobs-agent:0.1.0
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
