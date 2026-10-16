# Collector → server: report format (v1)

This is the contract between the collector running on every node and the server that receives its
data. The Go definition is [collector/internal/report/report.go](../collector/internal/report/report.go).
A JSON Schema for validation is [report.schema.json](report.schema.json).
Real examples from an NVIDIA H100 node are in [examples/](examples/), and [server-guide.md](server-guide.md)
explains what a server should do with them.

## HTTP

Every node sends one report per reporting window, every 10 s by default.

```
POST <endpoint URL, e.g. https://dashboard.example.com/v1/reports>
Content-Type: application/json
Authorization: Bearer <token>          (when a token is configured)
User-Agent: klique-idle-collector/<version>
Content-Encoding: gzip                 (only when the collector runs with --gzip)

<one JSON report>
```

The endpoint is any URL the operator configures. `/v1/reports` is the suggested path.

| Server answers | Collector does |
|---|---|
| any `2xx` (`202 Accepted` is fine) | done, sends the next report |
| `400`, `413`, `415`, `422` | drops this report (retrying the same bytes can't help) and moves on |
| anything else (`401`, `403`, `404`, `429`, `5xx`, timeouts) | keeps it and retries with backoff: 1 s, 2 s, 4 s… up to 60 s, with jitter |

Delivery guarantees:
- **At least once.** A retried report keeps its `report_id`, so de-duplicate on it.
- **In order per node.** Each node sends oldest first.
- **Bounded buffer.** If the server is unreachable, each node keeps up to 360 reports (1 h at 10 s) in memory. When the buffer is full, the oldest are dropped, and `collector.dropped_reports` counts them.
- **Late data is normal.** After an outage, a node sends its backlog quickly. Use `window` for time, not the arrival time.

A report is about 3 KB for an 8-GPU node, or under 1 KB gzipped.

## Identity

| Key | Meaning |
|---|---|
| `cluster` + `node.name` | A node. `node.name` is the Kubernetes node name, or the hostname. |
| `gpus[].uuid` | A physical GPU; stable across reboots and driver updates |
| `collector.instance_id` | One run of the collector; changes on every restart |
| `collector.seq` | 1, 2, 3… within an instance; a gap means reports were lost |

## Fields

Top level:
- `schema` is always `"klique-idle.report.v1"` for this version.
- `report_id`
- `created_at`
- `cluster`
- `errors` (optional) holds node-level problems as plain sentences. Examples: the driver is missing, the scheduler can't be reached, processes aren't visible. Show them as node health.

`collector`:
- `version`, `instance_id`, `started_at`, `seq`
- `gpu_source`: `nvml` for real GPUs, **`fake` for simulated data (demos only: don't mix it into real totals)**, or `none`.
- `redacted`: when true, names are hashed (`h:3f2a…`). Hashes are stable within a cluster, so grouping still works.
- `dropped_reports`

`node`:
- `name`, `labels` (operator-defined, e.g. `site`), `arch`, `kernel`, `os`
- `cpu.model` (often empty on ARM), `cpu.logical_cores`
- `memory_total_bytes`
- `gpu_driver.version`, `gpu_driver.cuda_version`

`window`:
- `start`, `end`: UTC. Windows are aligned to the clock (`:00`, `:10`, `:20`…), so windows from different nodes line up. The first and last window of a run can be shorter.
- `samples`: how many readings were taken, every `sample_interval_seconds` (2 s).

`host` (optional): whole-machine usage over the window.
- `cpu_util_percent.{avg,max}`: 0–100 across all cores.
- `memory_used_bytes.{avg,max}`

`gpus[]`: one entry per physical GPU.

| Field | Notes |
|---|---|
| `index`, `uuid`, `name`, `pci_bus_id` | `name` is the model as the driver reports it, e.g. `"NVIDIA H100 80GB HBM3"`; map it to your benchmark tables |
| `memory_total_bytes` | |
| `mig_enabled` | MIG-partitioned GPUs have no utilization (`util_percent` is null) |
| `util_percent.{avg,max}` | share of time a kernel was running, 0–100, from every driver reading in the window (several per second), so short bursts are not missed. **null = unknown**, not idle |
| `memory_used_bytes.{avg,max}` | framebuffer in use, excluding what the driver reserves. null = unknown |
| `power_watts`, `temperature_c` | `{avg,max}`, optional |
| `processes[]` | every process seen on the GPU in the window: `pid`, `name`, `user`, `memory_used_bytes` (peak), `owner`, and `cmdline` only if the operator enabled it |
| `allocated`, `allocated_to` | optional: whether the scheduler has given this GPU to a job, and which job. Currently filled on Slurm |
| `errors` | per-GPU problems |

`owner` (on processes, and on `allocated_to`) says which workload a process belongs to:

| `kind` | `id` | other fields |
|---|---|---|
| `kubernetes` | pod UID | `name` (pod), `namespace`, `container`, `container_id` |
| `slurm` | job ID | `name` (job name), `user`, `queue` (partition) |
| `pbs` | job ID (`1234.server`) | `name`, `user`, `queue` |
| `docker`, `podman` | — | `container_id` |
| `systemd` | unit, e.g. `vllm.service` | |
| `session` | `session-3` | `user` (uid) |

`owner` is missing when the process can't be attributed.

`scheduler`:

| Field | Meaning |
|---|---|
| `kind` | `kubernetes`, `slurm`, `pbs` or `none` |
| `gpus_allocatable` | GPUs the scheduler can hand out on this node (optional) |
| `gpus_allocated` | GPUs currently given to pods/jobs (optional) |
| `state` | scheduler node state, e.g. Slurm `IDLE` / `MIXED` / `DRAIN`, PBS `free` / `job-busy` (optional) |

## Deriving "idle" (suggested)

The collector sends measurements; deciding what counts as idle is the server's job, so the
thresholds can change without touching any node. A reasonable start:

1. **Classify each GPU in each window:**
   - **idle (free):** `util_percent.max < 5` and `memory_used_bytes.max < 5%` of `memory_total_bytes`.
   - **idle, memory held:** `util_percent.max < 5`, but memory is in use. Usually a forgotten notebook or a hung job. It's waste, but not usable until reclaimed.
   - **busy:** otherwise.
   - **unknown:** `util_percent` is null, or no report arrived. Don't count this as idle.
2. **Require persistence.** Count a GPU as idle only after N consecutive idle windows, e.g. 10 minutes (60 windows). Make it configurable.
3. **Idle GPU-hours** = sum of the idle window lengths (`window.end - window.start`).
4. **Whole node idle** = all GPUs of the node idle in the same window. This matters for models that need a full 8-GPU node per replica.
5. **Allocated but idle** (`allocated: true` and idle) is waste inside a job. **Idle and not allocated** is free capacity.

The tokens and $ calculation (GPU model → tokens/s → AWS Bedrock price) also belongs on the server.

## Compatibility

Within `klique-idle.report.v1`, fields are only ever **added**. Ignore fields you don't know.
Renaming, removing or changing the meaning of a field means a new `schema` value, and the server
can then reply `422` to versions it doesn't support.

## Schema history

Newest first. Every change to the report gets a line here.

| Date | Schema | Change |
|---|---|---|
| 2026-10-16 | `klique-idle.report.v1` | First version |

## Test data without GPUs

- `cd collector && make run-fake`: prints reports from 8 simulated GPUs to the terminal (works on macOS).
- `cd collector && make mock-receiver`: a stand-in server on `:8080` that logs each report. `GET /v1/nodes` returns the latest report per node.
- `cd collector && make kind-demo`: a local 3-node Kubernetes cluster with simulated GPUs sending to the mock receiver.
- Point a collector at your own server: `klique-idle-collector --gpu fake --endpoint http://localhost:8000/v1/reports --cluster dev`.
