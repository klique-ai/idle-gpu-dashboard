# collector

The `klique-idle-collector` agent that runs on every GPU node, and `klique-idle-hub`, the per-cluster
service the collectors report to (Prometheus metrics, optional Klique Cloud forwarding). Both are Go
and ship in one image; the hub is a static binary with no GPU code.

## How it works

```
every 2 s (aligned to the clock)          every 10 s (aligned: :00, :10, …)
  gpu.Source.Sample()   NVML or fake        window → report.Report → sender
  host.Reader.Sample()  /proc/stat, meminfo                        ├─ HTTP POST (queue, retry, backoff)
  owner.Resolver        /proc/<pid>/cgroup → pod / job / container └─ stdout (no endpoint, or --once)
  scheduler.Watcher     names + allocation, refreshed every 30 s in the background
```

| Package | Role |
|---|---|
| `internal/report` | The JSON contract. Read this first. |
| `internal/gpu` | `Source` interface. `NVML` reads real GPUs: full-coverage utilization via driver sample buffers, retries if the driver appears later, fallback library paths. `Fake` simulates busy/idle/memory-held phases. |
| `internal/host` | Whole-machine CPU and memory from procfs (host-wide even inside a container) |
| `internal/owner` | PID → process name, user, and owning workload, parsed from the cgroup path (Kubernetes, Slurm, PBS, Docker, Podman, systemd) |
| `internal/scheduler` | Kubernetes (API over plain HTTPS), Slurm (`scontrol`, `squeue`), PBS (`pbsnodes`, `qstat`): names, GPUs allocatable/allocated, per-GPU job (Slurm) |
| `internal/collector` | The loop: samples into windows, builds reports, redaction |
| `internal/sender` | Bounded queue of encoded reports (oldest dropped), retry with jittered backoff, permanent vs transient errors; `Pool` for parallel, per-node-ordered delivery (hub); stdout writer |
| `internal/config` | Flags + `KLIQUE_IDLE_*` env vars, validation |
| `internal/envflag` | Every flag as an env var (`KLIQUE_IDLE_*` for the collector, `KLIQUE_IDLE_HUB_*` for the hub) |
| `internal/health` | `/healthz`: loop liveness plus delivery stats |
| `internal/hub` | Keeps the latest report per node; `/metrics` (Prometheus text format, no client library), `/v1/nodes`, forwarding through `internal/sender` |
| `cmd/klique-idle-hub` | The hub binary: `--token`, `--cloud-api-key[-file]`, `--cloud-workers`, `--stale-after`, `--dump` |

## Develop

Works on macOS (cgo is required; NVML is mocked in tests, and `--gpu fake` replaces it at runtime).

```sh
make test                                   # unit tests, race detector
make run-fake                               # print reports from simulated GPUs
make hub                                    # terminal 1: the hub on :8080, logging every report
go run ./cmd/klique-idle-collector --gpu fake --endpoint http://localhost:8080/v1/reports --cluster dev   # terminal 2
curl -s localhost:8080/metrics              # what Prometheus would scrape
```

On macOS the host stats are missing, since there's no `/proc`. They show up in the report's
`errors`, which is expected. Run in Linux (Docker, kind) for the full picture.

## Build and release

| Target | Output |
|---|---|
| `make dist` | `dist/linux_{amd64,arm64}/klique-idle-collector` and `klique-idle-hub` |
| `make bundle` | `dist/klique-idle-collector-<version>-linux-<arch>.tar.gz`: both binaries, `install.sh`, systemd unit, plus `SHA256SUMS` |
| `make image` | multi-arch image `$(IMAGE):$(VERSION)`; `PUSH=1` pushes. Distroless, non-root; the entrypoint is the collector, the hub is `/usr/local/bin/klique-idle-hub` |

The [Dockerfile](Dockerfile) builds on AlmaLinux 8 so binaries need only an old glibc, and the build
fails if that regresses. The non-native architecture builds under emulation (fast with Docker
Desktop's Rosetta).

## Testing on real GPUs

`--once` against `nvidia-smi` is the quickest check. On Kubernetes:

```sh
kubectl -n klique-idle exec daemonset/klique-idle-collector -- klique-idle-collector --once
kubectl -n gpu-operator exec ds/nvidia-driver-daemonset -- nvidia-smi
```

For a GPU load to attribute, `nvcr.io/nvidia/cloud-native/dcgm:4.4.2-1-ubuntu22.04` with
`dcgmproftester13 --no-dcgm-validation -t 1004 -d 60` and `nvidia.com/gpu: 1` works
(`dcgmproftester12` for CUDA 12 drivers).
