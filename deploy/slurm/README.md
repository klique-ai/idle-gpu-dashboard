# Slurm

## Recommended: a service on every compute node

Install the collector as a systemd service on each GPU node with
[`../systemd/install.sh`](../systemd/install.sh), the same as on bare metal. It runs outside of
Slurm, so it:
- uses no job slot and no CPU allocation;
- sees every GPU (cgroup device limits only apply to jobs);
- reports even when the node is full.

```sh
# from a node that can reach all compute nodes, with the release bundle on a shared filesystem
pdsh -w gpu[01-64] 'sudo /shared/klique-idle/install.sh \
    --cluster hpc-a --endpoint https://dashboard.example.com/v1/reports \
    --token-file /shared/klique-idle/token --labels site=sjc-1'
```

Ansible, clush or your node image work just as well: the installer is idempotent.

What the collector adds on Slurm, detected automatically when `slurmd` runs on the node:

| | From |
|---|---|
| Which job owns each GPU process | the process's cgroup (`.../job_<id>/...`), no Slurm call needed |
| Job name, user and partition | `squeue --nodelist <node>` |
| GPUs allocatable and allocated on the node, and node state | `scontrol show node <node>` (`Gres`, `GresUsed`, `State`) |
| Which GPU index each job holds | `scontrol show job <id> --details` (`GRES=gpu:2(IDX:0-1)`) |

Notes:
- **Node name.** Slurm node names are taken to be the short hostname. Set `--env KLIQUE_IDLE_NODE_NAME=<name>` if yours differ.
- **Other users' jobs.** The service runs as an unprivileged user. If `slurm.conf` has `PrivateData=jobs`, `squeue` only shows that user's jobs, so names are missing (GPU usage is still reported). Install with `--run-as-root`, or make the `klique-idle` user a Slurm operator.
- **GPU index mapping.** GPU indexes are matched to NVML indexes. Both follow PCI bus order when the GPUs are configured with `AutoDetect=nvml` or listed in `/dev/nvidia<N>` order in `gres.conf`.

Verified against Slurm 23.11 (cgroup v2): node state, GPU counts, per-GPU job allocation and job
attribution from cgroups.

## Fallback: one batch job per node (no root needed)

[`submit-collectors.sh`](submit-collectors.sh) submits a long-running, 1-CPU, 200 MB job to each
node. Its environment comes from your shell, so this works without admin rights. It's good for a
quick trial, but it has real limits:

- **Hidden GPUs.** With `ConstrainDevices=yes` in `cgroup.conf` (common), a job that didn't request GPUs **cannot see them**. Reports will show no GPUs or an NVML error.
- **Exclusive nodes.** On partitions that allocate nodes exclusively, the job takes the whole node away from real work.
- **Time limits.** The job ends at the partition time limit and isn't resubmitted.
- **Full nodes.** While a node is fully allocated, the job waits in the queue and the node isn't reported.

```sh
./submit-collectors.sh --binary /shared/klique-idle/klique-idle-collector_linux_amd64 \
    --cluster hpc-a --endpoint https://dashboard.example.com/v1/reports \
    --token-file /shared/klique-idle/token --partition gpu --time 7-00:00:00
squeue --name=klique-idle-collector                          # check
scancel --name=klique-idle-collector --user=$USER            # stop
```

`--dry-run` prints the `sbatch` commands without submitting them.
