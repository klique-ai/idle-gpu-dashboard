package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/clearml/klique-idle-dashboard/collector/internal/report"
)

// fakeRun answers commands from a table keyed by "name arg1 arg2...".
func fakeRun(t *testing.T, answers map[string]string) runFunc {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		out, ok := answers[key]
		if !ok {
			t.Logf("unexpected command: %s", key)
			return nil, fmt.Errorf("%s: not found", key)
		}
		return []byte(out), nil
	}
}

func TestSlurmQuery(t *testing.T) {
	s := NewSlurm("gpu01")
	s.run = fakeRun(t, map[string]string{
		"scontrol show node gpu01 --details --oneliner": "NodeName=gpu01 Arch=x86_64 CoresPerSocket=32 CPUAlloc=24 CPUTot=128 " +
			"Gres=gpu:h100:8(S:0-1) GresUsed=gpu:h100:3(IDX:0-1,5) State=MIXED ThreadsPerCore=2 " +
			"CfgTRES=cpu=128,mem=1000000M,billing=128,gres/gpu=8 AllocTRES=cpu=24,mem=200G,gres/gpu=3\n",
		"squeue --noheader --nodelist gpu01 --states RUNNING --format %A|%j|%u|%P": "4242|pretrain|alice|gpu\n4243|notebook|bob|interactive\n",
		"scontrol show job 4242 --details --oneliner": "JobId=4242 JobName=pretrain UserId=alice(1000) NodeList=gpu[01-02] " +
			"TRES=cpu=32,gres/gpu=4 Nodes=gpu01 CPU_IDs=0-15 Mem=64000 GRES=gpu:h100:2(IDX:0-1) Nodes=gpu02 CPU_IDs=0-15 Mem=64000 GRES=gpu:h100:2(IDX:2-3)\n",
		"scontrol show job 4243 --details --oneliner": "JobId=4243 JobName=notebook NodeList=gpu01 Nodes=gpu01 CPU_IDs=16-23 Mem=8000 GRES=gpu(IDX:5)\n",
	})
	snap, err := s.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Node.State != "MIXED" || *snap.Node.GPUsAllocatable != 8 || *snap.Node.GPUsAllocated != 3 {
		t.Fatalf("node = %+v", snap.Node)
	}
	if w := snap.Workloads["4243"]; w.Name != "notebook" || w.User != "bob" || w.Queue != "interactive" {
		t.Fatalf("workload 4243 = %+v", w)
	}
	got := map[int]string{}
	for idx, o := range snap.GPUs {
		got[idx] = o.ID
	}
	want := map[int]string{0: "4242", 1: "4242", 5: "4243"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GPU allocation = %v, want %v", got, want)
	}
}

// Real Slurm 23.11 output, captured with two GPU jobs running. This site has
// no AccountingStorageTRES=gres/gpu, so the TRES fields carry no GPU counts.
func TestSlurmQueryRealOutput(t *testing.T) {
	s := NewSlurm("slurm-node")
	s.run = fakeRun(t, map[string]string{
		"scontrol show node slurm-node --details --oneliner": "NodeName=slurm-node CoresPerSocket=1  CPUAlloc=2 CPUEfctv=10 CPUTot=10 CPULoad=0.00 " +
			"AvailableFeatures=(null) ActiveFeatures=(null) Gres=gpu:4 GresDrain=N/A GresUsed=gpu:3 NodeAddr=slurm-node NodeHostName=slurm-node  " +
			"RealMemory=2000 AllocMem=200 FreeMem=N/A Sockets=10 Boards=1 State=MIXED ThreadsPerCore=1 TmpDisk=0 Weight=1 Owner=N/A " +
			"MCS_label=N/A Partitions=gpu  CfgTRES=cpu=10,mem=2000M,billing=10 AllocTRES=cpu=2,mem=200M CapWatts=n/a\n",
		"squeue --noheader --nodelist slurm-node --states RUNNING --format %A|%j|%u|%P": "3|pretrain|root|gpu\n4|notebook|root|gpu\n",
		"scontrol show job 3 --details --oneliner": "JobId=3 JobName=pretrain UserId=root(0) JobState=RUNNING NodeList=slurm-node " +
			"TresPerNode=gres/gpu:2 Nodes=slurm-node CPU_IDs=0 Mem=100 GRES=gpu:2(IDX:0-1) MinCPUsNode=1\n",
		"scontrol show job 4 --details --oneliner": "JobId=4 JobName=notebook UserId=root(0) JobState=RUNNING NodeList=slurm-node " +
			"TresPerNode=gres/gpu:1 Nodes=slurm-node CPU_IDs=1 Mem=100 GRES=gpu:1(IDX:2) MinCPUsNode=1\n",
	})
	snap, err := s.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Node.State != "MIXED" || snap.Node.GPUsAllocatable == nil || *snap.Node.GPUsAllocatable != 4 || *snap.Node.GPUsAllocated != 3 {
		t.Fatalf("node = %+v", snap.Node)
	}
	if snap.GPUs[0].ID != "3" || snap.GPUs[1].ID != "3" || snap.GPUs[2].Name != "notebook" || snap.GPUs[3] != nil {
		t.Fatalf("GPU allocation = %v", snap.GPUs)
	}
}

func TestGresGPUs(t *testing.T) {
	for in, want := range map[string]int{
		"gpu:4":                 4,
		"gpu:h100:8(S:0-1)":     8,
		"gpu:h100:3(IDX:0-1,5)": 3,
		"gpu:a100:2(IDX:0-1),gpu:v100:1(IDX:4),mps:100": 3,
		"gpu:(null):0(IDX:N/A)":                         0,
	} {
		if got, ok := gresGPUs(in); !ok || got != want {
			t.Errorf("gresGPUs(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "(null)", "mps:100"} {
		if _, ok := gresGPUs(in); ok {
			t.Errorf("gresGPUs(%q) found GPUs", in)
		}
	}
}

func TestSlurmHelpers(t *testing.T) {
	if got := expandRanges("0-2,5,7-8"); !reflect.DeepEqual(got, []int{0, 1, 2, 5, 7, 8}) {
		t.Errorf("expandRanges = %v", got)
	}
	for _, c := range []struct {
		list, node string
		want       bool
	}{
		{"gpu01", "gpu01", true},
		{"gpu[01-04,07]", "gpu03", true},
		{"gpu[01-04,07]", "gpu07", true},
		{"gpu[01-04,07]", "gpu05", false},
		{"gpu[01-04]", "gpu1", false}, // zero padding must match
		{"cpu[01-04]", "gpu01", false},
	} {
		if got := nodeListContains(c.list, c.node); got != c.want {
			t.Errorf("nodeListContains(%q, %q) = %v", c.list, c.node, got)
		}
	}
	if n, ok := tresGPUs("cpu=4,mem=1G,gres/gpu=2,gres/gpu:h100=2"); !ok || n != 2 {
		t.Errorf("tresGPUs = %d, %v", n, ok)
	}
	if _, ok := tresGPUs("cpu=4,mem=1G"); ok {
		t.Error("tresGPUs found GPUs in a CPU-only TRES")
	}
}

func TestPBSQuery(t *testing.T) {
	p := NewPBS("node01")
	p.run = fakeRun(t, map[string]string{
		"pbsnodes -v node01 -F json": `{"timestamp":1,"pbs_version":"23.06","nodes":{"node01":{"state":"job-busy",
			"jobs":["101.pbs-server/0","101.pbs-server/1","102.pbs-server/2"],
			"resources_available":{"ncpus":64,"ngpus":4,"mem":"512gb"},"resources_assigned":{"ncpus":8,"ngpus":3}}}}`,
		"qstat -f -F json 101.pbs-server 102.pbs-server": `{"Jobs":{"101.pbs-server":{"Job_Name":"cfd-run","Job_Owner":"carol@login1","queue":"gpuq"},
			"102.pbs-server":{"Job_Name":"STDIN","Job_Owner":"dave@login1","queue":"workq"}}}`,
	})
	snap, err := p.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Node.State != "job-busy" || *snap.Node.GPUsAllocatable != 4 || *snap.Node.GPUsAllocated != 3 {
		t.Fatalf("node = %+v", snap.Node)
	}
	if w := snap.Workloads["101.pbs-server"]; w.Name != "cfd-run" || w.User != "carol" || w.Queue != "gpuq" {
		t.Fatalf("workload = %+v", w)
	}
}

func TestKubernetesQuery(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/nodes/gpu-node-1":
			io.WriteString(w, `{"status":{"allocatable":{"cpu":"64","nvidia.com/gpu":"8"},"nodeInfo":{"osImage":"Ubuntu 22.04.4 LTS"}}}`)
		case r.URL.Path == "/api/v1/pods" && r.URL.Query().Get("fieldSelector") == "spec.nodeName=gpu-node-1":
			io.WriteString(w, `{"items":[
			 {"metadata":{"uid":"u1","name":"train-0","namespace":"ml"},
			  "spec":{"containers":[{"name":"main","resources":{"limits":{"nvidia.com/gpu":"4"}}},{"name":"sidecar","resources":{}}]},
			  "status":{"phase":"Running","containerStatuses":[{"name":"main","containerID":"containerd://abc"}]}},
			 {"metadata":{"uid":"u2","name":"done-job","namespace":"ml"},
			  "spec":{"containers":[{"name":"main","resources":{"limits":{"nvidia.com/gpu":"2"}}}]},
			  "status":{"phase":"Succeeded"}},
			 {"metadata":{"uid":"u3","name":"init-heavy","namespace":"ml"},
			  "spec":{"initContainers":[{"name":"warm","resources":{"requests":{"nvidia.com/gpu":"2"}}}],
			          "containers":[{"name":"main","resources":{"limits":{"nvidia.com/gpu":"1"}}}]},
			  "status":{"phase":"Pending"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := &KubernetesSource{nodeName: "gpu-node-1", baseURL: srv.URL, tokenFile: tokenFile, client: srv.Client()}
	snap, err := k.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *snap.Node.GPUsAllocatable != 8 || *snap.Node.GPUsAllocated != 6 { // 4 + max(1, init 2); Succeeded pod ignored
		t.Fatalf("node = allocatable %d allocated %d", *snap.Node.GPUsAllocatable, *snap.Node.GPUsAllocated)
	}
	if snap.NodeOS != "Ubuntu 22.04.4 LTS" {
		t.Fatalf("NodeOS = %q", snap.NodeOS)
	}

	o := &report.Owner{Kind: Kubernetes, ID: "u1", ContainerID: "abc"}
	snap.Enrich(o)
	if o.Name != "train-0" || o.Namespace != "ml" || o.Container != "main" {
		t.Fatalf("enriched owner = %+v", o)
	}

	// Auth failures surface the HTTP status.
	os.WriteFile(tokenFile, []byte("wrong"), 0o600)
	if _, err := k.Query(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
}

type stubSource struct {
	snaps []Snapshot
	errs  []error
	i     int
}

func (s *stubSource) Kind() string { return Slurm }
func (s *stubSource) Query(context.Context) (Snapshot, error) {
	i := s.i
	s.i++
	return s.snaps[i], s.errs[i]
}

func TestWatcherKeepsLastGoodSnapshot(t *testing.T) {
	two := 2
	src := &stubSource{
		snaps: []Snapshot{{Node: report.Scheduler{GPUsAllocated: &two}, Workloads: map[string]Workload{"1": {Name: "a"}}}, {}},
		errs:  []error{nil, errors.New("slurmctld unreachable")},
	}
	w := NewWatcher(src, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.Refresh(context.Background())
	w.Refresh(context.Background())
	snap := w.Snapshot()
	if snap.Err != "slurmctld unreachable" || snap.Workloads["1"].Name != "a" || *snap.Node.GPUsAllocated != 2 {
		b, _ := json.Marshal(snap)
		t.Fatalf("snapshot after failure = %s", b)
	}
	if snap.Node.Kind != Slurm {
		t.Fatalf("kind = %q", snap.Node.Kind)
	}
}
