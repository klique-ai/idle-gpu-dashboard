package collector

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"
)

// redact replaces user, workload and process names with stable hashes, so a
// receiver can still group by them without learning them. Hashes are salted
// with the cluster name: equal inside a cluster, unlinkable across clusters.
// Node names, GPU models and all numbers are kept.
func redact(r *report.Report, salt string) {
	h := func(s string) string {
		if s == "" {
			return ""
		}
		sum := sha256.Sum256([]byte(salt + "\x00" + s))
		return "h:" + hex.EncodeToString(sum[:6])
	}
	owner := func(o *report.Owner) {
		if o == nil {
			return
		}
		o.Name, o.Namespace, o.Container, o.User = h(o.Name), h(o.Namespace), h(o.Container), h(o.User)
		// Pod UIDs, job and container IDs are opaque; unit and session names
		// are chosen by people and often contain user or project names.
		if o.Kind == "systemd" || o.Kind == "session" {
			o.ID = h(o.ID)
		}
	}
	for i := range r.GPUs {
		g := &r.GPUs[i]
		owner(g.AllocatedTo)
		for j := range g.Processes {
			p := &g.Processes[j]
			p.Name, p.User, p.Cmdline = h(p.Name), h(p.User), ""
			owner(p.Owner)
		}
	}
}
