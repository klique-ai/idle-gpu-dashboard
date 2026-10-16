# Klique Idle Dashboard

See how much GPU capacity your clusters leave idle, and what it could be producing instead.

A small **collector** runs on every GPU node (Kubernetes, bare metal, Slurm or PBS). It reads GPU,
CPU and memory usage and works out which pod or job owns each GPU. It then sends a short JSON
report to a **server**, which turns the data into the dashboard.

```
 GPU node                                    GPU node
┌──────────────────────────────┐            ┌──────────────────────────────┐
│ klique-idle-collector         │            │ klique-idle-collector         │
│  NVML → GPU use, processes    │   ...      │  (DaemonSet pod, systemd     │
│  /proc → CPU, memory, owners  │            │   service or batch job)      │
│  scheduler → names, allocation│            │                              │
└──────────────┬───────────────┘            └──────────────┬───────────────┘
               │  HTTPS POST, one JSON report every 10 s    │
               └──────────────────────┬─────────────────────┘
                                      ▼
                         server + dashboard UI (separate)
```

## Status

| Part | State |
|---|---|
| Collector (Go, single binary, amd64 + arm64) | ready; unit-tested; validated on a real NVIDIA H100 (utilization, memory, power and process attribution match `nvidia-smi`) |
| Kubernetes (Helm chart, DaemonSet) | ready; tested on an H100 node with NVIDIA GPU Operator v25.3.4 and v26.7.1, CDI on and off, vanilla and hardened ([results](docs/gpu-operator-compatibility.md)), and on a 3-node kind cluster |
| Bare metal (systemd service + installer) | ready; validated on an H100 VM with a host-installed driver (580.173.02): numbers match `nvidia-smi`, processes attributed to their systemd unit, runs non-root; install, upgrade and uninstall tested |
| Slurm | scheduler integration tested against Slurm 23.11; no-root batch-job fallback tested |
| PBS (OpenPBS / PBS Professional) | draft: written against documented `pbsnodes`/`qstat` JSON, not run against a live PBS yet |
| Server and dashboard UI | not part of this collector; the data contract is [docs/payload.md](docs/payload.md) |

## Quick start

### Kubernetes

Requirements: NVIDIA GPU nodes with the driver and NVIDIA container toolkit. The NVIDIA GPU
Operator installs both.

```sh
helm install klique-idle-collector ./charts/klique-idle-collector \
  --namespace klique-idle --create-namespace \
  --set image.repository=<your-registry>/klique-idle-collector --set image.tag=<version> \
  --set cluster=prod-eu-1 \
  --set endpoint=https://dashboard.example.com/v1/reports \
  --set existingSecret.name=klique-idle-token   # a Secret with key "token"
```

- **Where it runs.** The DaemonSet runs on nodes labelled `nvidia.com/gpu.present=true`, which GPU Operator nodes are. Set `nodeSelector` if your labels differ.
- **How it reads GPUs.** Through the NVIDIA runtime, with `NVIDIA_VISIBLE_DEVICES=all` and utility capability only. It **does not request `nvidia.com/gpu`**, so it never takes a GPU away from workloads.
- **What it needs.** `hostPID` (to match GPU processes to pods), read-only RBAC (get nodes, list pods) and roughly 15 MB of memory. It runs as non-root on a read-only filesystem with no capabilities.

See [charts/klique-idle-collector/values.yaml](charts/klique-idle-collector/values.yaml) for every option.

### Bare metal (also the recommended way on Slurm and PBS nodes)

```sh
tar xzf klique-idle-collector-<version>-linux-amd64.tar.gz
cd klique-idle-collector-<version>-linux-amd64
sudo ./install.sh --cluster hpc-a --endpoint https://dashboard.example.com/v1/reports \
                  --token-file /root/klique-token --labels site=sjc-1
```

The installer:
1. Creates an unprivileged `klique-idle` user.
2. Installs a hardened systemd service.
3. Runs a self-test that shows what the node would report.
4. Starts the service.

Re-running it upgrades in place, and `--uninstall` removes it. To roll it out to many nodes, use
your usual tool, e.g. `pdsh -w gpu[01-64] 'sudo /shared/kic/install.sh ...'`.

### Slurm and PBS

Install the service on every node as above. The collector detects the scheduler and adds job
names and GPU allocation. If you can't install a service on the nodes, there is a no-root fallback
that runs the collector as a batch job, with limitations. See [deploy/slurm](deploy/slurm/README.md)
and [deploy/pbs](deploy/pbs/README.md).

### Try it without GPUs

```sh
cd collector
make run-fake        # reports from 8 simulated GPUs, printed to the terminal
make kind-demo       # local 3-node Kubernetes cluster + simulated GPUs + a mock server
```

Simulated reports are marked `"gpu_source": "fake"`.

## What leaves the node

One JSON document per window ([format](docs/payload.md), [example](docs/examples/report-h100-busy.json)):

- **Node:** name, OS, kernel, CPU model and count, memory total, NVIDIA driver version, and your `--labels`.
- **Host:** CPU and memory use (average and peak).
- **Per GPU:** model, UUID, memory total, utilization and memory use (average and peak), power, temperature.
- **Per GPU process:** PID, name, user, GPU memory, and the owning pod (name, namespace) or job (ID, name, user, queue).
- **Scheduler view:** GPUs allocatable and allocated; on Slurm, which job holds which GPU.

Names are included by default. `--redact` replaces them with hashes, which stay stable within a
cluster so grouping still works. Process command lines are **not** sent unless you pass
`--include-cmdline`, because they can contain secrets passed as arguments.

The collector is read-only:
- It never creates a CUDA context and never changes GPU settings.
- It doesn't write to disk.
- Its only network traffic is to the endpoint, plus the Kubernetes API on Kubernetes.
- On Slurm and PBS it also runs `scontrol` / `squeue` (or `pbsnodes` / `qstat`).

## Configuration

`klique-idle-collector --help` lists every flag. Each flag can also be set as an environment
variable: `--report-interval` → `KLIQUE_IDLE_REPORT_INTERVAL`. Flags win over the environment.

| Flag | Default | |
|---|---|---|
| `--cluster` | | cluster name on the dashboard (required with an endpoint) |
| `--endpoint` | | URL to POST reports to; empty prints them to stdout |
| `--token-file` / `--token` | | bearer token |
| `--labels` | | e.g. `site=sjc-1,rack=b4`, sent with every report |
| `--sample-interval` | `2s` | how often the node is read |
| `--report-interval` | `10s` | how often a report is sent |
| `--gpu` | `nvml` | `nvml`, `fake` (simulated) or `none` |
| `--scheduler` | `auto` | `auto`, `kubernetes`, `slurm`, `pbs` or `none` |
| `--redact` | `false` | hash user, job, pod and process names |
| `--include-cmdline` | `false` | send GPU process command lines |
| `--queue-size` | `360` | reports kept while the server is unreachable |
| `--gzip`, `--ca-file`, `--insecure-skip-verify` | | delivery options |
| `--health-addr` | `127.0.0.1:9478` | `/healthz` endpoint; `off` disables it |
| `--once` | | print one report and exit: exactly what would be sent |

## Operating it

- **See what a node sends:**
  - Kubernetes: `kubectl -n klique-idle exec daemonset/klique-idle-collector -- klique-idle-collector --once`
  - Service: `sudo -u klique-idle bash -c 'set -a; . /etc/klique-idle-collector/collector.env; klique-idle-collector --once'`
- **Health:** `curl -s localhost:9478/healthz` returns the loop state, the last report and delivery stats (pending, sent, dropped, last error).
- **Problems show up in the reports themselves** (`errors`) and in the logs:

| Message | Meaning |
|---|---|
| `NVIDIA driver library libnvidia-ml.so.1 not found` | No driver, or on Kubernetes the pod didn't get the NVIDIA runtime: check `nvidia.runtimeClassName`. The collector keeps retrying, so a driver that loads later is picked up. |
| `GPU processes are not visible in /proc` | Enable `hostPID` on Kubernetes; on hosts with `/proc` mounted `hidepid=2`, install with `--run-as-root`. |
| `NVML init: Insufficient Permissions` | `/dev/nvidia*` isn't readable by the collector's non-root user. The installer adds the user to the devices' group automatically; on Kubernetes, set `podSecurityContext.runAsUser=0`. |
| `kubernetes API ... 403` | RBAC is missing (`rbac.create=true`). |
| `cannot start health endpoint ... address already in use` | Another collector is already running on the node. |

## Development

```
collector/            Go module: the collector and a mock receiver
  cmd/                klique-idle-collector, mock-receiver
  internal/           report (data contract), gpu (NVML + simulator), host, owner,
                      scheduler (kubernetes, slurm, pbs), sender, collector, config, health
charts/               Helm chart
deploy/               systemd unit + installer, Slurm and PBS guides
docs/                 data contract and examples
```

```sh
cd collector
make test            # unit tests (race detector); runs on macOS too
make dist            # Linux amd64 + arm64 binaries
make bundle          # per-arch tarballs: binary + install.sh + systemd unit
make image           # multi-arch container image
```

The collector talks to NVML through NVIDIA's [go-nvml](https://github.com/NVIDIA/go-nvml), which
needs cgo. Release binaries are built on AlmaLinux 8 (glibc 2.28) and the build checks the glibc
floor. Current binaries need glibc ≥ 2.17, so they run on any mainstream Linux of the last
decade.

## License

Apache 2.0, see [LICENSE](LICENSE).
