# 신규 클러스터 온보딩 runbook

본 문서는 netobs 관측 스택 (netobs-agent 와 gpuobs-agent 와 correlation-exporter 와 rca-summarizer) 을 새 Kubernetes 클러스터에 올리는 절차다. 컴포넌트별 복사용 템플릿 (`deploy/<comp>/overlays/_template`) 과 일괄 배포 스크립트 (`scripts/deploy-cluster.sh`, #288) 를 전제로 하며, overlay 이름 규약은 `deploy/<comp>/overlays/<cluster>` 다. dev 전용 workload-injector 와 Grafana sidecar 의존인 dashboards 패키지는 본 절차 밖이다.

## 전제 조건

배포 전에 타깃 클러스터가 다음을 갖춰야 한다. 스크립트 preflight 가 일부를 점검하지만 생성하지는 않는다.

- 커널 BTF: 관측 대상 노드의 커널이 `/sys/kernel/btf/vmlinux` 를 노출해야 netobs-agent 의 eBPF 로드가 가능하다 (커널 5.4+ 에서 `CONFIG_DEBUG_INFO_BTF=y`)
- NVIDIA runtime: GPU 노드에 NVIDIA driver 와 container toolkit (GPU Operator 또는 수동 설치) 이 있어야 gpuobs-agent 가 NVML 을 질의한다
- Prometheus Operator: kube-prometheus-stack 류가 설치되어 `servicemonitors` 와 `prometheusrules` 와 `prometheuses` CRD 가 존재해야 한다
- 로컬 도구: `kubectl` (kustomize 내장) 과 대상 클러스터 kube context

## 클러스터별 변경값

템플릿을 복사한 뒤 `CHANGEME` 주석 자리를 아래 표대로 채운다. 표에 없는 값은 base 기본값을 따른다.

| 값 | 위치 | 기본값 | 설명 |
|---|---|---|---|
| operator release 라벨 | 각 컴포넌트 `kustomization.yaml` 의 `labels` | `kube-prometheus-stack` | operator 의 `serviceMonitorSelector` / `ruleSelector` 가 보는 라벨. `helm ls -n monitoring` 의 release 이름과 일치해야 하며 어긋나면 메트릭과 rule 이 조용히 누락된다. `includeSelectors: false` 라 selector 에는 주입되지 않는다 |
| `PROMETHEUS_URL` | correlation 과 rca-summarizer 의 `patch-configmap.yaml` | in-cluster kube-prometheus-stack service | 타깃 클러스터의 Prometheus 주소. release 이름과 네임스페이스에 따라 service 명이 달라진다 |
| `RECONCILE_INTERVAL` | correlation 의 `patch-configmap.yaml` | `5m` | correlation 분석 cadence. series 수가 많은 대형 클러스터는 늘려 fetch 부하를 줄인다 |
| `NIC_CAPACITY_BYTES_PER_SEC` | netobs 의 `patch-daemonset.yaml` | `1.25e9` (10GbE) | network throughput score 정규화 분모. 25GbE 는 `3.125e9` |
| `GPU_POLL_INTERVAL` | gpuobs 의 `patch-daemonset.yaml` | `5s` | NVML 폴링 주기. GPU 수가 많은 노드는 늘린다 |
| gpuobs `nodeSelector` | gpuobs 의 `patch-daemonset.yaml` | `accelerator: nvidia` + opt-in 라벨 | gpuobs 를 GPU 노드로 한정하는 스케줄 라벨. GPU 노드 라벨 스킴이 다른 클러스터 (예: `nvidia.com/gpu.present`) 는 이 값을 맞춘다. non-GPU 노드 상주는 NVML init 실패 후 비활성 pod 만 남는 낭비다. GPU 노드의 `nvidia.com/gpu` taint 는 base toleration 이 허용한다 (#295) |
| CUDA/NCCL 라이브러리 경로 | gpuobs 의 `patch-daemonset.yaml` (env) | (empty = 자동 순회) | 미지정 시 Debian multiarch 와 RHEL lib64 와 GPU Operator driver 컨테이너 후보를 순회해 첫 실존 경로에 attach 한다 (#296). 후보 밖 특수 경로만 `GPUOBS_CUDA_LIBCUDA_PATH` 와 `GPUOBS_NCCL_LIB_PATH` 로 고정한다 |
| 이미지 `newTag` | 각 `kustomization.yaml` | 현재 `VERSION` | `make bump` 이 overlays 전체를 자동 갱신하므로 손대지 않는다 |

## 절차

1. 템플릿 복사. `<cluster>` 는 배포 스크립트에 넘길 overlay 이름이 된다.

   ```sh
   for comp in netobs gpuobs correlation rca-summarizer; do
     cp -r "deploy/$comp/overlays/_template" "deploy/$comp/overlays/<cluster>"
   done
   grep -rn CHANGEME deploy/*/overlays/<cluster>/
   ```

2. 위 변경값 표대로 `CHANGEME` 자리를 채우고 `kubectl kustomize deploy/<comp>/overlays/<cluster>` 로 렌더를 확인한다.

3. 관측 대상 노드를 라벨링한다. netobs 는 opt-in 라벨, gpuobs 는 GPU 라벨까지 요구한다.

   ```sh
   kubectl --context <ctx> label nodes <node...> observability.netobs/enabled=true
   kubectl --context <ctx> label nodes <gpu-node...> accelerator=nvidia
   ```

4. GHCR pull secret 을 생성한다. namespace 는 netobs base 가 생성하지만 secret 을 먼저 두려면 namespace 를 선생성한다.

   ```sh
   kubectl --context <ctx> create namespace ebpf-project --dry-run=client -o yaml | kubectl --context <ctx> apply -f -
   kubectl --context <ctx> create secret docker-registry ghcr-creds -n ebpf-project \
     --docker-server=ghcr.io --docker-username=<github-user> --docker-password=<PAT(read:packages)>
   ```

5. 일괄 배포를 실행한다. preflight 가 context 연결과 CRD 와 release 라벨 정합을 fail-fast 로 점검하고, 의존 순서 배포 후 rollout 과 Prometheus 채택까지 검증한다.

   ```sh
   make deploy-cluster ENV=<cluster> CONTEXT=<ctx>
   ```

6. 배포 후 확인. 스크립트의 자동 검증 (netobs-agent target 과 recording rule 채택) 에 더해 다음을 수동 확인한다.

   ```sh
   # 에이전트 target 이 모두 up 인지. 쿼리는 up{job=~"netobs-agent|gpuobs-agent"} 의 URL 인코딩으로,
   # 중괄호 등 RFC 비허용 문자를 apiserver 버전과 무관하게 안전히 전달한다.
   kubectl --context <ctx> get --raw \
     "/api/v1/namespaces/<monitoring-ns>/services/prometheus-operated:9090/proxy/api/v1/query?query=up%7Bjob%3D~%22netobs-agent%7Cgpuobs-agent%22%7D"
   # 합성 API 가 응답하는지 (노드 health 와 pressure 가 채워지기까지 recording rule 5m 윈도우 필요)
   kubectl --context <ctx> -n ebpf-project port-forward svc/correlation-exporter 9830:9830 &
   curl -s "http://127.0.0.1:9830/api/v1/overview"
   ```

## 문제 해결

- preflight 가 release 라벨 불일치로 실패하면 `helm ls -n <monitoring-ns>` 의 release 이름을 각 overlay `labels` 에 반영한다
- 메트릭이 비는데 target 은 up 이면 recording rule 채택을 확인한다 (`/api/v1/rules` 에 `netobs-gpuobs.*` 그룹 존재 여부)
- `ImagePullBackOff` 면 `ghcr-creds` secret 과 PAT 권한 (`read:packages`) 을 확인한다
- gpuobs pod 가 스케줄되지 않으면 GPU 노드 라벨 2종 (`accelerator=nvidia`, `observability.netobs/enabled=true`) 을 확인한다
