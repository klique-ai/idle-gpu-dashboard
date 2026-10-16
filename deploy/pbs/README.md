# PBS (OpenPBS / PBS Professional)

> Status: **draft.** The PBS integration is written against PBS's documented JSON output
> (`pbsnodes -F json`, `qstat -f -F json`) and has unit tests, but it has not run against a live
> PBS cluster yet.

## Recommended: a service on every compute node

Same as bare metal: run [`../systemd/install.sh`](../systemd/install.sh) on each GPU node, e.g.
with `pdsh`. The collector detects PBS when `pbs_mom` runs on the node, or when `/etc/pbs.conf`
exists.

| | From |
|---|---|
| Which job owns each GPU process | the process's cgroup, when the PBS cgroups hook is enabled (`.../pbs_jobs.service/jobid/<id>`) |
| Job name, owner and queue | `qstat -f -F json <job ids>` |
| GPUs available and assigned on the node, and node state | `pbsnodes -v <node> -F json` (`resources_available.ngpus`, `resources_assigned.ngpus`) |

Notes:
- **Node name.** The PBS vnode name is taken to be the hostname. Set `--env KLIQUE_IDLE_NODE_NAME=<vnode>` if it differs.
- **Without the cgroups hook,** job processes aren't in per-job cgroups, so processes are reported with their user but not their job.
- **Per-GPU job allocation** isn't extracted yet: only node-level counts.

## Fallback: one batch job per node (no root needed)

[`submit-collectors.sh`](submit-collectors.sh) submits one long-running 1-CPU job per node with
`qsub`. It has the same limits as the Slurm fallback:
- GPUs may be hidden from a job that didn't request them (cgroups hook with device isolation).
- Exclusive placement takes the whole node.
- The job ends at its walltime.

```sh
./submit-collectors.sh --binary /shared/klique-idle/klique-idle-collector_linux_amd64 \
    --cluster pbs-a --endpoint https://dashboard.example.com/v1/reports \
    --token-file /shared/klique-idle/token --queue workq --walltime 168:00:00
```

`--dry-run` prints the `qsub` commands without submitting them.
