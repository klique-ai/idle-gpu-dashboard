# Example reports

Every file is one complete report, exactly as a collector POSTs it. `go test ./internal/report/`
(part of `make test`) fails if any of them no longer matches the Go types in
[report.go](../../collector/internal/report/report.go); the JSON Schema
[report.schema.json](../report.schema.json) is checked against the same types.

| File | Source | How to refresh |
|---|---|---|
| `report-h100-busy.json` | Real: NVIDIA H100 80GB on Kubernetes (GPU Operator v25.3.4), during a 96% GPU load in a pod | Capture on a real GPU (below) |
| `report-h100-idle.json` | Real: the same GPU a minute later, idle | Capture on a real GPU (below) |
| `report-simulated-4gpu.json` | Simulated GPUs (`gpu_source: "fake"`): one idle, one busy, two idle but holding memory | `cd collector && make examples` |

## Capturing real examples

On a GPU node with the collector deployed, run a GPU load in a pod, then:

```sh
kubectl -n klique-idle exec daemonset/klique-idle-collector -- klique-idle-collector --once > busy.json
```

Replace the identifiers before committing: cluster, node name, labels, GPU UUID, pod names and IDs.
The H100 examples use `prod-eu-1`, `gpu-node-01` and `ml-research/llm-finetune-7d9f8`.
