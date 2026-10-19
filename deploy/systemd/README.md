# systemd: bare metal, Slurm and PBS nodes

[install.sh](install.sh) installs the collector as a hardened systemd service on one machine. The
release bundle (`make bundle` in `collector/`) contains the binary, this script and
[klique-idle-collector.service](klique-idle-collector.service).

```sh
sudo ./install.sh --cluster hpc-a --labels site=sjc-1 --listen :9478   # Prometheus metrics on :9478/metrics
sudo ./install.sh                      # later: upgrade the binary, keep the configuration
sudo ./install.sh --uninstall --purge  # remove everything
```

Where the data goes:
- **Prometheus / Grafana (default).** Every node serves its own GPU and host metrics at `/metrics`,
  by default on `127.0.0.1:9478` (this node only). `--listen :9478` lets a Prometheus on another host
  scrape `http://<node>:9478/metrics`; nothing leaves the node unless it does. Add the nodes to a
  scrape job and import the dashboard (see [docs/grafana.md](../../docs/grafana.md)).
- **Klique Cloud (optional).** With an account, also send every report there; the metrics stay on:
  ```sh
  sudo ./install.sh --cluster hpc-a --endpoint https://idle.klique.ai/v1/reports --token-file /root/klique-key
  ```
  The same works for any server that speaks the [report format](../../docs/payload.md). Later
  reconfigures keep the endpoint and token unless you pass `--endpoint none`.

The metrics hold GPU and host numbers per node (no user, job, pod or process names), like
node-exporter or dcgm-exporter, and `/healthz` shows the collector's state. Before `--listen :9478`
on a host with a public address, make sure a firewall limits port 9478 to your Prometheus;
`--listen off` turns both off.

What it does:
1. Creates the system user `klique-idle`. If `/dev/nvidia*` isn't world-accessible, it adds the user to the devices' group.
2. Installs `/usr/local/bin/klique-idle-collector`.
3. Writes `/etc/klique-idle-collector/collector.env` (and `token`, with `--endpoint`), mode 0640, owned `root:klique-idle`.
4. Runs `--once` as the service user and prints what the node would report: GPUs found, scheduler, problems. **Nothing is sent** during this self-test.
5. Enables and starts the service.

The unit runs the collector unprivileged with:
- a read-only view of the filesystem and no capabilities;
- memory capped at 256 MB, CPU at 20%, `Nice=10`.

`PrivateDevices` and `ProtectProc` are deliberately left off: they would hide the GPUs and the
GPU processes.

Configuration is the env file: any `KLIQUE_IDLE_<FLAG>` from `klique-idle-collector --help`.
Edit it, then `systemctl restart klique-idle-collector`.

| Need | Option |
|---|---|
| `/proc` mounted with `hidepid=2`, or Slurm `PrivateData=jobs` | `--run-as-root` |
| Extra settings | `--env KLIQUE_IDLE_REPORT_INTERVAL=30s` (repeatable) |
| Logs / health / metrics | `journalctl -u klique-idle-collector -f`, `curl -s localhost:9478/healthz`, `curl -s localhost:9478/metrics` |

**Not for GPU Operator nodes.** There the driver lives in a container and the host has no
`/dev/nvidia*`, so use the Helm chart. The collector reports exactly this if installed on such a
node.

Tested on Ubuntu 24.04:
- install, upgrade, run-as-root switch and uninstall;
- on an NVIDIA H100 VM with a host-installed driver (580.173.02): utilization, memory, power and processes matched `nvidia-smi`; a GPU process started as a systemd unit was attributed to `gpu-busy.service`; the service ran unprivileged in about 34 MB of memory.
