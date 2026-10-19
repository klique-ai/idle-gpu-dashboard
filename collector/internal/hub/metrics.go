package hub

import (
	"io"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/metrics"
)

func (h *Hub) writeMetrics(w io.Writer) {
	m := metrics.NewSet()
	nodes := h.fresh()
	for _, e := range nodes {
		m.AddReport(&e.rep)
	}
	received, rejected := h.counters()
	m.Gauge("klique_idle_hub_nodes", "Nodes that reported recently.", nil, float64(len(nodes)))
	m.Counter("klique_idle_hub_reports_received_total", "Reports accepted from collectors.", nil, float64(received))
	m.Counter("klique_idle_hub_reports_rejected_total", "Reports refused (bad JSON, unknown schema).", nil, float64(rejected))
	if h.opt.Forward != nil {
		s := h.opt.Forward.Stats()
		m.Gauge("klique_idle_hub_forward_pending", "Reports waiting to be forwarded to Klique Cloud.", nil, float64(s.Pending))
		m.Counter("klique_idle_hub_forward_sent_total", "Reports forwarded to Klique Cloud.", nil, float64(s.Sent))
		m.Counter("klique_idle_hub_forward_dropped_total", "Reports dropped instead of forwarded (queue full or rejected).", nil, float64(s.Dropped))
	}
	m.Write(w)
}
