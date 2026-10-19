# CLAUDE.md

Guidance for working on this repository (people and coding agents alike).

## What this is

Klique Idle Dashboard shows how much GPU capacity a cluster leaves idle and what it could produce
instead. This repo is the open-source part:
- the **collector**, a per-node agent that reports GPU, CPU and memory usage plus the owning pod or job of every GPU process, and serves its node's metrics at `/metrics` on the health port (how bare metal, Slurm and PBS reach Prometheus; on Kubernetes scrape the hub instead);
- the **hub**, one per cluster, which receives the collectors' reports, serves them as Prometheus metrics with a Grafana dashboard (the default), and forwards them to Klique Cloud (the hosted dashboard) only when given an API key.

Any server can consume the reports; the contract is [docs/payload.md](docs/payload.md), and
[docs/server-guide.md](docs/server-guide.md) is the brief for building one.

```
collector/   Go module: klique-idle-collector + klique-idle-hub (one image)
charts/      Helm chart `klique-idle`: collector DaemonSet + hub, Grafana dashboard JSON (charts/klique-idle/dashboards/)
deploy/      systemd unit + installer (bare metal, Slurm, PBS nodes); Slurm/PBS guides and batch-job fallbacks
docs/        report format, server guide, Grafana, real example reports, GPU Operator compatibility
```

## Commands

```sh
cd collector
make test         # unit tests with -race; works on macOS (cgo + NVML mocks, no GPU needed)
make vet          # go vet + gofmt check
make run-fake     # print reports from simulated GPUs
make hub          # the hub on :8080 (POST /v1/reports, GET /metrics, GET /v1/nodes)
make kind-demo    # 3-node local Kubernetes + simulated GPUs + the hub (switches your kubectl context)
make dist         # Linux amd64/arm64 binaries (Docker buildx)
make bundle       # per-arch tarballs: binary + install.sh + systemd unit
make image        # multi-arch image (PUSH=1 to push)
helm lint ../charts/klique-idle --set cluster=x
```

`klique-idle-collector --once` prints exactly one report and exits. It's the fastest way to see what
a node sends.

## Rules

- **The report format is a public contract.** Within `klique-idle.report.v1`, only *add* fields. Renaming, removing or changing meaning requires a new schema value. When fields change, update `docs/payload.md` and regenerate the examples.
- **Read-only, always.** The collector must never create CUDA contexts, change GPU settings or write to disk.
- **Few dependencies:** the standard library plus `github.com/NVIDIA/go-nvml`. Kubernetes is reached with plain HTTPS; there's no client-go. The hub uses no GPU code and builds without cgo; Prometheus output is written by hand, with no client library.
- **Nothing leaves the cluster unless the operator asks.** Klique Cloud forwarding happens only with an API key (`cloud.apiKey` / `--cloud-api-key`). Keep it opt-in, and keep the default endpoint the public `https://idle.klique.ai/v1/reports`.
- **This repo is public.** No internal hostnames, registries, account IDs or links to private repos, in code, docs or history.
- **The Grafana side stays small.** A fixed set of gauges (one series per GPU or node; nothing per process or pod), one dashboard, no alerts or recording rules. The hosted dashboard is the product; Grafana is the simple, safe default.
- **Safe defaults:**
  - non-root, read-only filesystem, no capabilities;
  - process command lines only with `--include-cmdline`, because they can contain secrets;
  - names are sent by default, and `--redact` hashes them.
- **Every flag has an env var** (`--report-interval` → `KLIQUE_IDLE_REPORT_INTERVAL`). Kubernetes and systemd configure through env.
- **Error messages say what to do.** They end up in reports and in front of operators.

## Changing the report

`collector/internal/report/report.go` is the source of truth for what collectors send. Tests in
`internal/report/schema_test.go` fail when it drifts from `docs/report.schema.json` (fields,
types, required/optional, nullable) or from the files in `docs/examples/`. When you change a report
type:

1. **Additive change** (new optional field, the usual case): keep `Schema` as is.
   **Breaking change** (rename, removal, new meaning, new required field): bump `Schema` in report.go to `klique-idle.report.v2`; servers rely on it.
2. Update `docs/report.schema.json` until `go test ./internal/report/` passes. Give new fields a `description`.
3. Document the field in `docs/payload.md`, and add a row to its **Schema history** table (date, schema, change).
4. Run `make examples` to regenerate the simulated example. The real H100 examples can't be regenerated without a GPU: update them by hand for an additive change, or recapture them (`docs/examples/README.md`) for a breaking one.
5. Mention the change to whoever runs the server.

## Build facts

- **cgo is required.** go-nvml needs it and relies on GNU ld: NVML symbols are left unresolved and bound lazily after `dlopen`. Zig's linker rejects that, so don't switch toolchains.
- **Release binaries are built on AlmaLinux 8 (glibc 2.28).** The Dockerfile fails if a newer glibc symbol appears. Current binaries need glibc ≥ 2.17 (arm64) or ≥ 2.3.3 (amd64).
- **Local builds need `CGO_CFLAGS=-Wno-deprecated-declarations`** to silence NVIDIA header warnings; the Makefile sets it.

## Lessons from real hardware (Nebius H100, GPU Operator)

- **Reading GPUs without requesting one.** The chart sets `NVIDIA_VISIBLE_DEVICES=all` and *also* mounts `/dev/null` at `/var/run/nvidia-container-devices/all`. The Klique-documented GPU Operator config disables the env-var path for unprivileged pods (`ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED=false`); the volume mount is what works there. Verified both ways.
- **Driver upgrades.** The collector holds the GPU open, which blocks the GPU Operator from unloading the old driver ("driver is in use"). The chart's node affinity makes the pod leave the node while it's labelled `nvidia.com/gpu.deploy.*=paused-for-driver-upgrade`. Verified: a 595 → 580 swap completed unattended in about 3 minutes. Don't remove this rule.
- **The GPU Operator matrix** (v25.3.4 and v26.7.1, CDI on and off, vanilla and hardened) all pass; see docs/gpu-operator-compatibility.md.
- **Driver-container nodes.** On GPU Operator nodes the driver lives in a container (`/run/nvidia/driver`). The host has the library but no `/dev/nvidia*`, so the bare-metal binary can't read GPUs there; it reports a clear error pointing to the DaemonSet.
- **Truncated process names.** `/proc/<pid>/comm` is cut at 15 characters; the full name comes from argv[0].
- **Utilization readings.** NVML `GetSamples(GPU_UTILIZATION_SAMPLES)` returns percent and matched `nvidia-smi` on the H100.
- **Slurm GPU counts.** Read them from `Gres`/`GresUsed`. `CfgTRES`/`AllocTRES` only list GPUs when the site sets `AccountingStorageTRES=gres/gpu`.
- **Single-node test clusters.** Node replacement can deadlock on PodDisruptionBudgets (coredns, metrics-server). Give node groups a `max_surge` rollout.
