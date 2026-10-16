# klique-idle-collector Helm chart

Runs the collector as a DaemonSet on every GPU node. Every option is documented in
[values.yaml](values.yaml).

```sh
helm install klique-idle-collector ./charts/klique-idle-collector \
  --namespace klique-idle --create-namespace \
  --set image.repository=<your-registry>/klique-idle-collector --set image.tag=<version> \
  --set cluster=prod-eu-1 \
  --set endpoint=https://dashboard.example.com/v1/reports \
  --set existingSecret.name=klique-idle-token      # Secret with key "token"
```

There is no public image yet: build and push one with `cd collector && make image IMAGE=<your-registry>/klique-idle-collector PUSH=1`.
With GitOps tools (Argo CD, `helm template`) also set `nvidia.runtimeClassName=nvidia`, because
auto-detection needs a live cluster.

## How it gets at the GPUs

The pod reads GPUs through NVML **without requesting `nvidia.com/gpu`**, so it never takes a GPU
away from workloads.
- `runtimeClassName: auto` picks the `nvidia` RuntimeClass when it exists, which the GPU Operator creates.
- `NVIDIA_VISIBLE_DEVICES=all` with only the `utility` capability: no compute access.
- A read-only `/dev/null` mount at `/var/run/nvidia-container-devices/all`. This is the device plugin's own way of requesting GPUs, and it's needed where the GPU Operator is hardened to ignore the env var from unprivileged pods (the setup ClearML/Klique document). Turn it off with `nvidia.requestViaVolumeMount=false`.
- `hostPID: true` lets it match GPU processes to pods. Without it, usage is still reported, but not who is using the GPU.
- Read-only RBAC (get nodes, list pods) for pod names and GPU allocation.
- Non-root, read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp. About 15 MB of memory.
- **Driver upgrades:** the pod leaves a node while the GPU Operator marks it `paused-for-driver-upgrade`, so it doesn't block unloading the old driver (`nvidia.pauseDuringDriverUpgrade`).

## Common settings

| Value | Default | |
|---|---|---|
| `cluster` | (required) | name on the dashboard |
| `endpoint` | | where to send reports; empty prints them to the pod log |
| `token` / `existingSecret` | | bearer token, mounted as a file |
| `labels` | `{}` | extra labels per report, e.g. `{site: sjc-1}` |
| `collector.reportInterval` | `10s` | |
| `collector.gpu` | `nvml` | `fake` simulates GPUs (demos, clusters without GPUs) |
| `collector.redact` | `false` | hash user, pod and process names |
| `nodeSelector` | `nvidia.com/gpu.present: "true"` | label from NVIDIA GPU Feature Discovery; set `null` to run everywhere |
| `mockReceiver.enabled` | `false` | in-cluster stand-in server for demos; used automatically when `endpoint` is empty |

## Demo without GPUs

```sh
helm install klique-idle-collector ./charts/klique-idle-collector -n klique-idle --create-namespace \
  --set cluster=demo --set collector.gpu=fake --set nodeSelector=null \
  --set mockReceiver.enabled=true --set token=demo
kubectl -n klique-idle logs -f deploy/klique-idle-collector-mock-receiver
```

## Troubleshooting

`kubectl -n klique-idle exec daemonset/klique-idle-collector -- klique-idle-collector --once` prints
what the node would send, including `errors`.

| Symptom | Fix |
|---|---|
| DaemonSet has 0 pods | Your GPU nodes lack `nvidia.com/gpu.present=true`: set `nodeSelector` |
| `NVIDIA driver library ... not found` | The NVIDIA runtime isn't applied: check `kubectl get runtimeclass`, set `nvidia.runtimeClassName` |
| Pod fails with `RuntimeClass "nvidia" not found` | Set `nvidia.runtimeClassName=""` when nvidia is the node's default runtime |
| `GPU processes are not visible` | Keep `hostPID: true` |
| `kubernetes API ... 403` | Keep `rbac.create=true` |
