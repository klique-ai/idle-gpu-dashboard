package owner

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"
)

const cid = "4f1c9a2b3d4e5f60718293a4b5c6d7e8f9010203a4b5c6d7e8f90102a3b4c5d6"

func TestParseCgroup(t *testing.T) {
	pod := "1b2c3d4e-5f60-7182-93a4-b5c6d7e8f901"
	podU := "1b2c3d4e_5f60_7182_93a4_b5c6d7e8f901"
	tests := []struct {
		name    string
		content string
		want    *report.Owner
	}{
		{"k8s v2 systemd driver",
			"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podU + ".slice/cri-containerd-" + cid + ".scope\n",
			&report.Owner{Kind: KindKubernetes, ID: pod, ContainerID: cid}},
		{"k8s v2 seen through a cgroup namespace",
			"0::/../../kubepods-besteffort-pod" + podU + ".slice/cri-containerd-" + cid + ".scope",
			&report.Owner{Kind: KindKubernetes, ID: pod, ContainerID: cid}},
		// Captured on a kind node (kubelet in a nested systemd slice).
		{"k8s kind node, host view",
			"0::/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-burstable.slice/kubelet-kubepods-burstable-pod99547781_858d_43ec_9714_03e000c9f21e.slice/cri-containerd-d0a2f762ff9bf40e929734ef719e05b21c9a4a632a97d7c1e48e3eb038196f5a.scope\n",
			&report.Owner{Kind: KindKubernetes, ID: "99547781-858d-43ec-9714-03e000c9f21e", ContainerID: "d0a2f762ff9bf40e929734ef719e05b21c9a4a632a97d7c1e48e3eb038196f5a"}},
		{"k8s kind node, seen from the collector pod (hostPID)",
			"0::/../../kubelet-kubepods-burstable-pod99547781_858d_43ec_9714_03e000c9f21e.slice/cri-containerd-d0a2f762ff9bf40e929734ef719e05b21c9a4a632a97d7c1e48e3eb038196f5a.scope\n",
			&report.Owner{Kind: KindKubernetes, ID: "99547781-858d-43ec-9714-03e000c9f21e", ContainerID: "d0a2f762ff9bf40e929734ef719e05b21c9a4a632a97d7c1e48e3eb038196f5a"}},
		{"k8s v1 cgroupfs driver",
			"12:memory:/kubepods/burstable/pod" + pod + "/" + cid + "\n11:cpu,cpuacct:/kubepods/burstable/pod" + pod + "/" + cid + "\n1:name=systemd:/kubepods/burstable/pod" + pod + "/" + cid,
			&report.Owner{Kind: KindKubernetes, ID: pod, ContainerID: cid}},
		{"k8s guaranteed pod with cri-o",
			"0::/kubepods.slice/kubepods-pod" + podU + ".slice/crio-" + cid + ".scope",
			&report.Owner{Kind: KindKubernetes, ID: pod, ContainerID: cid}},
		{"slurm cgroup v2",
			"0::/system.slice/slurmstepd.scope/job_4242/step_0/user/task_0",
			&report.Owner{Kind: KindSlurm, ID: "4242"}},
		{"slurm 23.11 batch step (captured)",
			"0::/system.slice/slurm-node_slurmstepd.scope/job_3/step_batch/user/task_special\n",
			&report.Owner{Kind: KindSlurm, ID: "3"}},
		{"slurm cgroup v2 with node-named scope",
			"0::/system.slice/gpu01_slurmstepd.scope/job_77/step_batch/user/task_0",
			&report.Owner{Kind: KindSlurm, ID: "77"}},
		{"slurm cgroup v1",
			"5:devices:/slurm/uid_1000/job_4242/step_batch/task_0\n4:memory:/slurm/uid_1000/job_4242/step_batch\n3:freezer:/slurm/uid_1000/job_4242/step_batch",
			&report.Owner{Kind: KindSlurm, ID: "4242"}},
		{"pbs",
			"0::/pbs_jobs.service/jobid/1234.pbs-server",
			&report.Owner{Kind: KindPBS, ID: "1234.pbs-server"}},
		{"pbs array subjob",
			"0::/pbs_jobs.service/jobid/1234[5].pbs.example.com",
			&report.Owner{Kind: KindPBS, ID: "1234[5].pbs.example.com"}},
		{"docker cgroup v1",
			"12:memory:/docker/" + cid,
			&report.Owner{Kind: KindDocker, ContainerID: cid}},
		{"docker cgroup v2",
			"0::/system.slice/docker-" + cid + ".scope",
			&report.Owner{Kind: KindDocker, ContainerID: cid}},
		{"rootless podman",
			"0::/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-" + cid + ".scope",
			&report.Owner{Kind: KindPodman, ContainerID: cid}},
		{"systemd service",
			"0::/system.slice/vllm-server.service",
			&report.Owner{Kind: KindSystemd, ID: "vllm-server.service"}},
		{"login session",
			"0::/user.slice/user-1000.slice/session-3.scope",
			&report.Owner{Kind: KindSession, ID: "session-3", User: "1000"}},
		{"root cgroup", "0::/", nil},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCgroup(tt.content)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("ParseCgroup() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFullName(t *testing.T) {
	for _, c := range []struct{ comm, cmdline, want string }{
		// Seen on an H100 node: comm is cut at 15 characters.
		{"dcgmproftester1", "/usr/bin/dcgmproftester13\x00--no-dcgm-validation\x00", "dcgmproftester13"},
		{"python3", "/usr/bin/python3\x00train.py\x00", "python3"},          // not truncated
		{"vllm-entrypoint", "python3\x00-m\x00vllm\x00", "vllm-entrypoint"}, // renamed process: keep comm
		{"kworker/u8:1-ev", "", "kworker/u8:1-ev"},                          // kernel thread
	} {
		if got := fullName(c.comm, []byte(c.cmdline)); got != c.want {
			t.Errorf("fullName(%q, %q) = %q, want %q", c.comm, c.cmdline, got, c.want)
		}
	}
}

func TestLookup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "4321")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"comm":    "python3\n",
		"status":  "Name:\tpython3\nUid:\t1000\t1000\t1000\t1000\nGid:\t1000\t1000\t1000\t1000\n",
		"cmdline": "python3\x00train.py\x00--lr\x000.1\x00",
		"cgroup":  "0::/system.slice/slurmstepd.scope/job_9/step_0/user/task_0\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := NewResolver(root, true)
	r.lookupUser = func(uid string) (*user.User, error) {
		if uid == "1000" {
			return &user.User{Username: "alice"}, nil
		}
		return nil, errors.New("unknown")
	}
	p, err := r.Lookup(4321)
	if err != nil {
		t.Fatal(err)
	}
	want := Process{Name: "python3", User: "alice", Cmdline: "python3 train.py --lr 0.1", Owner: &report.Owner{Kind: KindSlurm, ID: "9"}}
	if p.Name != want.Name || p.User != want.User || p.Cmdline != want.Cmdline || *p.Owner != *want.Owner {
		t.Fatalf("Lookup() = %+v (owner %+v), want %+v", p, p.Owner, want)
	}

	// Without --include-cmdline the command line is not sent.
	r.includeCmdline = false
	if p, _ := r.Lookup(4321); p.Cmdline != "" {
		t.Fatalf("cmdline read although disabled: %q", p.Cmdline)
	}

	// A truncated comm is completed from argv[0], still without sending the cmdline.
	os.WriteFile(filepath.Join(dir, "comm"), []byte("dcgmproftester1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "cmdline"), []byte("/usr/bin/dcgmproftester13\x00-t\x001004\x00"), 0o644)
	if p, _ := r.Lookup(4321); p.Name != "dcgmproftester13" || p.Cmdline != "" {
		t.Fatalf("Lookup() name=%q cmdline=%q", p.Name, p.Cmdline)
	}

	// Unknown user IDs fall back to the number.
	if got := r.userName("4242"); got != "4242" {
		t.Fatalf("userName(4242) = %q", got)
	}

	if _, err := r.Lookup(99999); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lookup(missing) err = %v, want not-exist", err)
	}
}
