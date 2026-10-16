# Docs

| File | For | What |
|---|---|---|
| [server-guide.md](server-guide.md) | server/UI developers | What the server receives, the MVP to build, how to derive idle time, tokens and money |
| [payload.md](payload.md) | server developers, integrators | The report format field by field, HTTP contract, compatibility rules |
| [report.schema.json](report.schema.json) | server developers | JSON Schema (draft 2020-12) of a report, for validation; checked against all examples and real H100 reports |
| [examples/](examples/) | everyone | Real reports from an H100 node (busy and idle) and a simulated 4-GPU node |
| [gpu-operator-compatibility.md](gpu-operator-compatibility.md) | operators | Which NVIDIA GPU Operator versions and settings the collector was tested with |

Deployment guides live next to what they deploy: [../charts/klique-idle-collector](../charts/klique-idle-collector/README.md),
[../deploy/systemd](../deploy/systemd/README.md), [../deploy/slurm](../deploy/slurm/README.md),
[../deploy/pbs](../deploy/pbs/README.md).
