# NVIDIA GPU Operator compatibility

Tested with the Helm chart's defaults on a Kubernetes 1.31 node with one NVIDIA H100 80GB HBM3
(Ubuntu 24.04), October 2026.

Each configuration was checked the same way:
1. The collector pod sees the GPU without requesting it.
2. A GPU load pod (`dcgmproftester13`, ~96% utilization) shows as busy, with the process attributed to the right pod and container, and the full process name.
3. The node shows 1 GPU allocated.
4. The report has no errors.
5. The GPU reads 0% with no processes once the load ends.

| GPU Operator | Driver | Device mode | Toolkit config | Result |
|---|---|---|---|---|
| v25.3.4 | 580.82.07 | legacy (`mode=auto`) | Klique-hardened¹ | ✅ 7/7 |
| v25.3.4 | 580.82.07 | legacy | vanilla (chart defaults) | ✅ 7/7 |
| v25.3.4 | 580.82.07 | CDI (`cdi.enabled=true`, `cdi.default=true`) | vanilla | ✅ 7/7 |
| v26.7.1 | 595.91.07 | CDI (default from v25.10; runtime `mode=cdi`, device plugin `cdi-annotations,cdi-cri`) | vanilla | ✅ 7/7 |
| v26.7.1 | 595.91.07 | legacy (`cdi.enabled=false`) | vanilla | ✅ 7/7 |
| v26.7.1 | 595.91.07 | legacy (`cdi.enabled=false`) | Klique-hardened¹ | ✅ 7/7 |

¹ The values from the Klique fractional-GPU documentation: the toolkit's
`ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED=false` plus
`ACCEPT_NVIDIA_VISIBLE_DEVICES_AS_VOLUME_MOUNTS=true`, and the device plugin's
`DEVICE_LIST_STRATEGY=volume-mounts`.

## What makes it work everywhere

- **Two GPU requests, one of which always counts.** The chart requests GPUs through `NVIDIA_VISIBLE_DEVICES=all`, which vanilla and CDI setups honour. It also mounts `/var/run/nvidia-container-devices/all`, which the hardened setup honours. Verified on the hardened config: with `nvidia.requestViaVolumeMount=false` the collector sees no GPU, and with it on it does.
- **Driver upgrades.** The collector holds the GPU open, and a driver change (here 595 → 580) needs the old kernel module unloaded. The chart's node affinity removes the pod while the operator labels the node `nvidia.com/gpu.deploy.*=paused-for-driver-upgrade`, and brings it back afterwards. The swap completed in about 3 minutes with the collector installed, unattended. Without this rule the driver pod failed with "driver is in use" and the upgrade hung.

## Not covered yet

- MIG-partitioned GPUs (reported with `util_percent: null` by design)
- Multi-GPU nodes on real hardware (multi-GPU paths are unit-tested and ran with simulated GPUs)
- Kubernetes nodes without the GPU Operator (host-installed driver + NVIDIA container toolkit). Hosts with a host-installed driver *outside* Kubernetes are covered: see [../deploy/systemd](../deploy/systemd/README.md).
- OpenShift
