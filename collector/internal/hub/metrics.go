package hub

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// The metrics are a small, fixed set of gauges built from the latest report
// of every node: one series per GPU or node, nothing per process or pod, so
// cardinality stays at the size of the cluster. A value the collector
// doesn't know (null in the report) is left out, never reported as 0.
//
// Values describe the last report window (10 s by default): averages, or the
// peak where idle detection needs it (utilization max, memory max).

type family struct {
	name, help, typ string
	lines           []string
}

type metricSet struct {
	order []*family
	byKey map[string]*family
}

func newMetricSet() *metricSet { return &metricSet{byKey: map[string]*family{}} }

func (m *metricSet) family(name, typ, help string) *family {
	f, ok := m.byKey[name]
	if !ok {
		f = &family{name: name, help: help, typ: typ}
		m.byKey[name] = f
		m.order = append(m.order, f)
	}
	return f
}

func (m *metricSet) gauge(name, help string, labels []string, v float64) {
	m.add(name, "gauge", help, labels, v)
}

func (m *metricSet) add(name, typ, help string, labels []string, v float64) {
	f := m.family(name, typ, help)
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i])
			b.WriteString(`="`)
			b.WriteString(escapeLabel(labels[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
	f.lines = append(f.lines, b.String())
}

func (m *metricSet) write(w io.Writer) {
	bw := bufio.NewWriter(w)
	for _, f := range m.order {
		bw.WriteString("# HELP " + f.name + " " + f.help + "\n")
		bw.WriteString("# TYPE " + f.name + " " + f.typ + "\n")
		for _, l := range f.lines {
			bw.WriteString(l + "\n")
		}
	}
	_ = bw.Flush()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (h *Hub) writeMetrics(w io.Writer) {
	m := newMetricSet()
	nodes := h.fresh()

	for _, e := range nodes {
		r := &e.rep
		nl := []string{"cluster", r.Cluster, "node", r.Node.Name}

		m.gauge("klique_idle_node_last_report_timestamp_seconds", "End of the node's last report window (Unix time).",
			nl, float64(r.Window.End.UnixMilli())/1000)
		m.gauge("klique_idle_node_memory_total_bytes", "Node RAM.", nl, float64(r.Node.MemoryTotalBytes))
		if r.Host != nil {
			m.gauge("klique_idle_node_cpu_utilization_percent", "Node CPU utilization over the last window, 0-100 across all cores.",
				nl, r.Host.CPUUtilPercent.Avg)
			m.gauge("klique_idle_node_memory_used_bytes", "Node RAM in use, averaged over the last window.",
				nl, float64(r.Host.MemoryUsedBytes.Avg))
		}
		if r.Scheduler.GPUsAllocated != nil {
			m.gauge("klique_idle_node_gpus_allocated", "GPUs the scheduler has given to pods or jobs on this node.",
				nl, float64(*r.Scheduler.GPUsAllocated))
		}
		m.gauge("klique_idle_node_errors", "Node-level problems in the last report (details: GET /v1/nodes).",
			nl, float64(len(r.Errors)))

		for _, g := range r.GPUs {
			gl := []string{"cluster", r.Cluster, "node", r.Node.Name, "gpu", strconv.Itoa(g.Index), "uuid", g.UUID, "model", g.Name}
			m.gauge("klique_idle_gpu_memory_total_bytes", "GPU framebuffer size.", gl, float64(g.MemoryTotalBytes))
			if g.UtilPercent != nil {
				m.gauge("klique_idle_gpu_utilization_percent", "Share of time a kernel was running, averaged over the last window (0-100).",
					gl, g.UtilPercent.Avg)
				m.gauge("klique_idle_gpu_utilization_max_percent", "Highest utilization reading in the last window (0-100).",
					gl, g.UtilPercent.Max)
			}
			if g.MemoryUsedBytes != nil {
				m.gauge("klique_idle_gpu_memory_used_bytes", "Peak GPU memory in use during the last window.",
					gl, float64(g.MemoryUsedBytes.Max))
			}
			if g.PowerWatts != nil {
				m.gauge("klique_idle_gpu_power_watts", "GPU power draw, averaged over the last window.", gl, g.PowerWatts.Avg)
			}
			if g.TemperatureC != nil {
				m.gauge("klique_idle_gpu_temperature_celsius", "GPU temperature, averaged over the last window.", gl, g.TemperatureC.Avg)
			}
			if g.Allocated != nil {
				m.gauge("klique_idle_gpu_allocated", "1 when the scheduler has given this GPU to a pod or job.", gl, b2f(*g.Allocated))
			}
			m.gauge("klique_idle_gpu_processes", "Processes seen on the GPU during the last window.", gl, float64(len(g.Processes)))
		}
	}

	received, rejected := h.counters()
	m.gauge("klique_idle_hub_nodes", "Nodes that reported recently.", nil, float64(len(nodes)))
	m.add("klique_idle_hub_reports_received_total", "counter", "Reports accepted from collectors.", nil, float64(received))
	m.add("klique_idle_hub_reports_rejected_total", "counter", "Reports refused (bad JSON, unknown schema).", nil, float64(rejected))
	if h.opt.Forward != nil {
		s := h.opt.Forward.Stats()
		m.gauge("klique_idle_hub_forward_pending", "Reports waiting to be forwarded to Klique Cloud.", nil, float64(s.Pending))
		m.add("klique_idle_hub_forward_sent_total", "counter", "Reports forwarded to Klique Cloud.", nil, float64(s.Sent))
		m.add("klique_idle_hub_forward_dropped_total", "counter", "Reports dropped instead of forwarded (queue full or rejected).", nil, float64(s.Dropped))
	}
	m.write(w)
}
