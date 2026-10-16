# Building the server: what it receives and what it should do

The collectors do the measuring. The server stores what they send and turns it into the dashboard:
idle capacity, and what that capacity could have produced.

This guide is the starting point for the server and UI. The exact field reference is
[payload.md](payload.md).

## What arrives

Every GPU node POSTs one JSON report every 10 seconds. Real examples, captured on an
NVIDIA H100 node on Kubernetes (identifiers anonymised):

| File | What it shows |
|---|---|
| [examples/report-h100-busy.json](examples/report-h100-busy.json) | GPU at 96% for a pod (`ml-research/llm-finetune-7d9f8`), the process and its GPU memory, 1 of 1 GPU allocated by Kubernetes |
| [examples/report-h100-idle.json](examples/report-h100-idle.json) | The same GPU a minute later: 0%, no processes; the scheduler block shows nothing allocated |
| [examples/report-simulated-4gpu.json](examples/report-simulated-4gpu.json) | A simulated 4-GPU node: busy, idle, and idle-but-holding-memory GPUs side by side |

Abridged, one GPU of a busy report looks like this:

```json
{
  "schema": "klique-idle.report.v1",
  "report_id": "…",
  "cluster": "prod-eu-1",
  "node": { "name": "gpu-node-01", "labels": { "site": "eu-north-1" }, "cpu": { "logical_cores": 16 }, "memory_total_bytes": 211069915136 },
  "window": { "start": "2026-10-16T10:15:20Z", "end": "2026-10-16T10:15:30Z", "sample_interval_seconds": 2, "samples": 5 },
  "gpus": [{
    "index": 0, "uuid": "GPU-…", "name": "NVIDIA H100 80GB HBM3", "memory_total_bytes": 85520809984,
    "util_percent": { "avg": 95.36, "max": 96 },
    "memory_used_bytes": { "avg": 1026555904, "max": 1026555904 },
    "processes": [{ "pid": 27487, "name": "dcgmproftester13", "user": "root", "memory_used_bytes": 1017118720,
                    "owner": { "kind": "kubernetes", "name": "llm-finetune-7d9f8", "namespace": "ml-research", "container": "trainer" } }]
  }],
  "scheduler": { "kind": "kubernetes", "gpus_allocatable": 1, "gpus_allocated": 1 }
}
```

## The MVP server in five steps

1. **Accept reports.** `POST /v1/reports`:
   - Check the `Authorization: Bearer <token>` header.
   - Accept `Content-Encoding: gzip` if you want smaller requests; collectors only send it with `--gzip`.
   - Reply `202`.
   - Reply `422` to a `schema` you don't support, and `400` to invalid JSON. Collectors drop those reports and retry anything else, so a server outage loses nothing.
2. **De-duplicate on `report_id`.** Retries resend the same report.
3. **Store one row per GPU per window,** keyed on `(cluster, node.name, gpus[].uuid, window.start)`. Keep:
   - window start and end;
   - `util_percent.max`;
   - `memory_used_bytes.max` and `memory_total_bytes`;
   - GPU `name`;
   - the process owners;
   - `allocated` if present.

   Keep node-level rows too (host CPU and memory, `scheduler`, `errors`, labels). At 10 s per GPU that's 8,640 rows per GPU per day, so downsample to e.g. 1-minute or hourly buckets after a few days.
4. **Classify each GPU window:**

   ```python
   def gpu_state(gpu, idle_util=5.0, idle_mem_frac=0.05):
       util, mem = gpu.get("util_percent"), gpu.get("memory_used_bytes")
       if util is None or mem is None:
           return "unknown"                      # never count unknown as idle
       if util["max"] >= idle_util:
           return "busy"
       if mem["max"] >= idle_mem_frac * gpu["memory_total_bytes"]:
           return "idle_memory_held"             # e.g. a forgotten notebook: waste, but not free
       return "idle"
   ```

   A GPU counts as idle only after it has been idle for long enough, e.g. 10 minutes (60 windows in a row). Keep the thresholds configurable.
5. **Aggregate for the dashboard:**
   - **Idle GPU-hours** per GPU, node, cluster or site: sum the lengths (`end - start`) of idle windows.
   - **Idle right now:** GPUs whose latest window is idle.
   - **Whole-node idle:** all GPUs of a node idle in the same window. This matters for models that need a full 8-GPU node.
   - **Collector health:** nodes whose last report is older than about 1 minute, or that have `errors`.

## From idle hours to tokens and money

The dashboard's headline numbers are what the idle GPUs *could* have produced, priced at what the
same tokens cost from a cloud API (AWS Bedrock on-demand output-token prices):

```
idle GPUs           = average idle fraction × number of GPUs
usable idle GPUs    = idle GPUs that fit the model:
                        1-GPU model  → all of them
                        8-GPU model  → only GPUs on fully idle nodes
replicas            = usable idle GPUs ÷ GPUs per replica
tokens per second   = replicas × throughput of one replica (benchmark per model and GPU type)
tokens per year     = tokens per second × 31,536,000
dollars per year    = tokens per year ÷ 1,000,000 × price per 1M tokens
```

- **Potential savings:** this formula on the last 30 days, annualised.
- **Left on the table:** the same formula on the recorded history; days without data count as zero.

The GPU type comes from `gpus[].name`, e.g. `"NVIDIA H100 80GB HBM3"`. Map it to your throughput
table (H100, H200, B200, L40S…).

## Treat these carefully

- **`collector.gpu_source: "fake"`** is simulated data from demos and tests. Keep it out of real totals, or show a "simulated" badge.
- **`null` means unknown, not zero.** This covers `util_percent: null` (e.g. MIG GPUs) and missing reports.
- **Late reports are normal.** After an outage a node sends up to an hour of backlog at once. Always place data by `window`, not by arrival time.
- **Names can be hashed.** With `collector.redacted: true`, user, pod and job names arrive as `h:…` hashes, which are stable within a cluster.

## Developing without GPUs

The collector simulates GPUs, so you can feed your server realistic data from a laptop. Each
collector instance below is one fake node:

```sh
cd collector
for n in 01 02 03; do
  go run ./cmd/klique-idle-collector --gpu fake --fake-gpus 8 --fake-gpu-model h100 \
    --cluster dev --node-name gpu-node-$n --labels site=lab --health-addr off \
    --endpoint http://localhost:8000/v1/reports --token dev-token &
done
```

Simulated GPUs switch between busy, idle and idle-with-memory-held every few minutes, so idle
detection has something to find. `make mock-receiver` shows what a minimal receiver logs, and
`make kind-demo` runs the whole Kubernetes setup locally.
