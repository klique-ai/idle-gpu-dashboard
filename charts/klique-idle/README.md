# klique-idle Helm chart

Runs the collector as a DaemonSet on every GPU node, and the hub that the collectors report to.
The hub serves Prometheus metrics (for Grafana) and forwards to Klique Cloud only when you give it
an API key. Every option is documented in [values.yaml](values.yaml).

```sh
helm install klique-idle oci://registry-1.docker.io/kliqueai/klique-idle \
  --namespace klique-idle --create-namespace --set cluster=prod-eu-1
```

Then choose where the data goes:
- **Grafana:** with kube-prometheus-stack, `--set metrics.serviceMonitor.enabled=true
  --set metrics.serviceMonitor.labels.release=<its release name> --set grafanaDashboard.enabled=true`;
  other setups in [docs/grafana.md](../../docs/grafana.md).
- **Klique Cloud:** `--set cloud.apiKey=<key>`, or `--set cloud.existingSecret.name=<Secret with key "api-key">`.
- **Your own server:** `--set endpoint=https://your-server/v1/reports --set hub.enabled=false`.

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

## The hub

One Deployment (`<release>-hub`, port 8080), non-root and without a service-account token:
- `POST /v1/reports` from the collectors;
- `GET /metrics` for Prometheus;
- `GET /v1/nodes`: the latest report of every node, as JSON (needs the token, if one is set);
- forwards each report to `cloud.endpoint` only when a Klique Cloud key is configured.

The collectors authenticate to the hub with a token. Unless you set `token` or `existingSecret`, the
chart generates one and keeps it across upgrades.

If the hub is briefly down, the collectors keep up to an hour of reports and resend them. Reports
waiting to be forwarded to Klique Cloud are held in the hub's memory (`--cloud-queue-size`, 10000 by
default) and drained for up to 20 s when it stops. Set `hub.extraEnv` (e.g. `HTTPS_PROXY`) when the
cluster reaches the internet through a proxy.

## Common settings

| Value | Default | |
|---|---|---|
| `cluster` | (required) | name on the dashboard |
| `cloud.apiKey` / `cloud.existingSecret` | | Klique Cloud API key; **forwarding is off without it** |
| `metrics.serviceMonitor.enabled` | `false` | ServiceMonitor for the hub (Prometheus Operator) |
| `grafanaDashboard.enabled` | `false` | dashboard ConfigMap for Grafana's sidecar |
| `hub.enabled` | `true` | the hub; collectors report to it unless `endpoint` is set |
| `endpoint` | | send reports straight to this URL instead of the hub |
| `token` / `existingSecret` | | bearer token the collectors send (and the hub then requires), mounted as a file |
| `labels` | `{}` | extra labels per report, e.g. `{site: sjc-1}` |
| `collector.reportInterval` | `10s` | |
| `collector.gpu` | `nvml` | `fake` simulates GPUs (demos, clusters without GPUs) |
| `collector.redact` | `false` | hash user, pod and process names |
| `nodeSelector` | `nvidia.com/gpu.present: "true"` | label from NVIDIA GPU Feature Discovery; set `null` to run everywhere |

## Demo without GPUs

```sh
helm install klique-idle oci://registry-1.docker.io/kliqueai/klique-idle -n klique-idle --create-namespace \
  --set cluster=demo --set collector.gpu=fake --set nodeSelector=null --set hub.dump=true
kubectl -n klique-idle logs -f deploy/klique-idle-hub          # every report as JSON
kubectl -n klique-idle port-forward svc/klique-idle-hub 8080    # then: curl -s localhost:8080/metrics
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
