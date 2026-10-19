# Grafana

On Kubernetes the hub serves the latest state of every node in Prometheus format at
`http://<hub>:8080/metrics`. On bare metal, Slurm and PBS every collector serves its own node at
`http://<node>:9478/metrics`, with the same metric names and labels. Scrape one or the other, not
both: on Kubernetes the collector pods also answer `/metrics`, and scraping them as well as the hub
counts every GPU twice.
Any Prometheus-compatible scraper can collect it (Prometheus, Grafana Alloy, VictoriaMetrics, the
OpenTelemetry Collector), and [the included dashboard](../charts/klique-idle/dashboards/klique-idle.json)
charts it in Grafana.

## Connect it

**kube-prometheus-stack** (Prometheus Operator + Grafana with the dashboard sidecar):

```sh
helm upgrade --install klique-idle oci://registry-1.docker.io/kliqueai/klique-idle -n klique-idle \
  --set cluster=prod-eu-1 \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.serviceMonitor.labels.release=<your kube-prometheus-stack release> \
  --set grafanaDashboard.enabled=true
```

The `release` label is needed because kube-prometheus-stack, by default, only picks up
ServiceMonitors labelled with its release name. The dashboard appears as **Klique Idle: GPUs** if
Grafana's dashboard sidecar watches the `klique-idle` namespace (or all namespaces).

Scrape every 10 s, the collectors' report interval (the ServiceMonitor does): the gauges describe
the last 10 s window, so a slower scrape skips windows.

**Any other Prometheus:** the hub Service has the usual `prometheus.io/scrape` annotations, or add a
job:

```yaml
scrape_configs:
  - job_name: klique-idle-hub
    scrape_interval: 10s
    static_configs:
      - targets: ['klique-idle-hub.klique-idle.svc:8080']
```

**Bare metal, Slurm, PBS:** scrape every GPU node, installed with `--listen :9478` so the metrics
are reachable from Prometheus (the default keeps them on the node):

```yaml
scrape_configs:
  - job_name: klique-idle
    scrape_interval: 10s
    static_configs:
      - targets: ['gpu-01:9478', 'gpu-02:9478']   # or file_sd / your inventory
```

**Grafana Cloud / Alloy:** scrape the same targets with `prometheus.scrape` and send them with
`prometheus.remote_write`.

Without the sidecar, import the dashboard by hand: *Dashboards → New → Import*, upload
`charts/klique-idle/dashboards/klique-idle.json`, pick your Prometheus data source.

## The dashboard

Totals of GPUs and nodes reporting, idle GPUs, GPUs allocated but idle, average utilization, idle
GPU-hours, utilization over time, and a table of every GPU. Filter by cluster and node.

**Idle** means under 5% utilization for the last 10 minutes. **Allocated but idle** counts, per
node, the GPUs the scheduler has handed out minus the GPUs that were busy (Kubernetes tells how
many GPUs a node gave to pods, not which ones). The thresholds live in the panels' queries, so you
can change them there.

## Metrics

All gauges, built from each node's last report (a 10 s window by default). A value the collector
doesn't know, such as utilization on MIG-partitioned GPUs, is missing rather than 0. Nodes that
stop reporting disappear after 2 minutes (`hub.staleAfter`).

GPU series carry `cluster`, `node`, `gpu` (index), `uuid` and `model`; node series carry `cluster`
and `node`.

| Metric | Meaning |
|---|---|
| `klique_idle_gpu_utilization_percent` | share of time a kernel was running, window average (0-100) |
| `klique_idle_gpu_utilization_max_percent` | highest reading in the window; use it for idle detection |
| `klique_idle_gpu_memory_used_bytes` | peak GPU memory in use in the window |
| `klique_idle_gpu_memory_total_bytes` | GPU memory size |
| `klique_idle_gpu_power_watts`, `klique_idle_gpu_temperature_celsius` | window averages |
| `klique_idle_gpu_allocated` | 1 when the scheduler gave this GPU to a job (Slurm; Kubernetes only reports a per-node count) |
| `klique_idle_gpu_processes` | processes seen on the GPU in the window |
| `klique_idle_node_cpu_utilization_percent`, `klique_idle_node_memory_used_bytes`, `klique_idle_node_memory_total_bytes` | host usage |
| `klique_idle_node_gpus_allocated` | GPUs the scheduler has handed out on the node |
| `klique_idle_node_errors` | node-level problems in the last report (details: `GET /v1/nodes`) |
| `klique_idle_node_last_report_timestamp_seconds` | end of the node's last report window |
| `klique_idle_hub_*` | the hub itself: nodes, reports received/rejected, Klique Cloud forwarding |
| `klique_idle_collector_reports_pending`, `klique_idle_collector_reports_dropped_total` | a collector's delivery to its endpoint (per-node `/metrics` only) |

There are no per-process or per-pod metrics, which keeps the number of series at the size of the
cluster. To see who is using each GPU, read `GET /v1/nodes` on the hub (with the token, if one is
set), run `klique-idle-collector --once` on a node, or use Klique Cloud.
