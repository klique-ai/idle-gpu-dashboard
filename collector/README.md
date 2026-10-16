# collector

The `klique-idle-collector` agent (Go), plus `mock-receiver`, a stand-in server for development.

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
| `internal/sender` | Bounded queue (oldest dropped), retry with jittered backoff, permanent vs transient errors; stdout writer |
| `internal/config` | Flags + `KLIQUE_IDLE_*` env vars, validation |
| `internal/health` | `/healthz`: loop liveness plus delivery stats |
| `cmd/mock-receiver` | Logs one line per report; `GET /v1/nodes` returns the latest report per node |

## Develop

Works on macOS (cgo is required; NVML is mocked in tests, and `--gpu fake` replaces it at runtime).

```sh
make test                                   # unit tests, race detector
make run-fake                               # print reports from simulated GPUs
make mock-receiver                          # terminal 1: stand-in server on :8080
go run ./cmd/klique-idle-collector --gpu fake --endpoint http://localhost:8080/v1/reports --cluster dev   # terminal 2
```

On macOS the host stats are missing, since there's no `/proc`. They show up in the report's
`errors`, which is expected. Run in Linux (Docker, kind) for the full picture.

## Build and release

| Target | Output |
|---|---|
| `make dist` | `dist/linux_{amd64,arm64}/klique-idle-collector` (+ `mock-receiver`) |
| `make bundle` | `dist/klique-idle-collector-<version>-linux-<arch>.tar.gz`: binary, `install.sh`, systemd unit, plus `SHA256SUMS` |
| `make image` | multi-arch image `$(IMAGE):$(VERSION)`; `PUSH=1` pushes. Distroless, non-root; also contains `mock-receiver` |

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
