#!/usr/bin/env bash
# Fallback for trying the collector on a Slurm cluster without root: submits
# one long-running, low-footprint job per node. Read deploy/slurm/README.md
# first. The recommended deployment is the systemd service on every node
# (deploy/systemd/install.sh), which has none of the limitations below:
#   - with ConstrainDevices=yes in cgroup.conf the job cannot see the GPUs;
#   - on nodes allocated exclusively the job occupies the whole node;
#   - the job ends at the partition's time limit and is not resubmitted;
#   - nothing is collected on a node while the job is pending.
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: submit-collectors.sh --binary PATH --cluster NAME --endpoint URL [options]

  --binary PATH       collector binary on a filesystem every node can read
  --cluster NAME      cluster name shown on the dashboard
  --endpoint URL      where to send reports
  --token-file FILE   token file readable on every node (shared filesystem)
  --partition NAME    partition to submit to and take nodes from (default: the default partition)
  --nodes HOSTLIST    nodes to cover, e.g. gpu[01-08] (default: all nodes of the partition)
  --time LIMIT        job time limit (default: the partition's maximum)
  --account NAME      account to charge, if your site requires one
  --dry-run           print the sbatch commands instead of running them

Stop them with: scancel --name=klique-idle-collector --user=$USER
EOF
}

die() { echo "error: $*" >&2; exit 1; }

binary='' cluster='' endpoint='' token_file='' partition='' nodes='' time_limit='' account='' dry_run=0
need_arg() { [[ $# -ge 2 && -n $2 ]] || die "$1 needs a value"; }
while [[ $# -gt 0 ]]; do
	case "$1" in
		--binary) need_arg "$@"; binary=$2; shift 2 ;;
		--cluster) need_arg "$@"; cluster=$2; shift 2 ;;
		--endpoint) need_arg "$@"; endpoint=$2; shift 2 ;;
		--token-file) need_arg "$@"; token_file=$2; shift 2 ;;
		--partition) need_arg "$@"; partition=$2; shift 2 ;;
		--nodes) need_arg "$@"; nodes=$2; shift 2 ;;
		--time) need_arg "$@"; time_limit=$2; shift 2 ;;
		--account) need_arg "$@"; account=$2; shift 2 ;;
		--dry-run) dry_run=1; shift ;;
		-h | --help) usage; exit 0 ;;
		*) usage >&2; die "unknown option: $1" ;;
	esac
done
[[ -n $binary && -n $cluster && -n $endpoint ]] || { usage >&2; die "--binary, --cluster and --endpoint are required"; }
[[ -x $binary ]] || die "$binary is not executable"
[[ -z $token_file || -r $token_file ]] || die "cannot read $token_file"
command -v sbatch >/dev/null || die "sbatch not found: run this on a Slurm login node"

# Submit to one partition, and only to its nodes: the default partition
# unless --partition is given.
if [[ -z $partition ]]; then
	partition=$(sinfo --noheader --format=%P | sed -n 's/\*$//p' | head -n1)
	[[ -n $partition ]] || die "no default partition found; pass --partition"
fi
if [[ -z $nodes ]]; then
	# --Node prints one line per node; a node can appear in several states.
	nodes=$(sinfo --noheader --Node --partition="$partition" --format=%N | sort -u | paste -sd, -)
	[[ -n $nodes ]] || die "no nodes found in partition $partition"
fi

export KLIQUE_IDLE_CLUSTER=$cluster KLIQUE_IDLE_ENDPOINT=$endpoint KLIQUE_IDLE_SCHEDULER=slurm KLIQUE_IDLE_HEALTH_ADDR=off
[[ -n $token_file ]] && export KLIQUE_IDLE_TOKEN_FILE=$token_file

count=0 failed=0
for node in $(scontrol show hostnames "$nodes" | sort -u); do
	# shellcheck disable=SC2054 # the comma belongs to sbatch's --export syntax
	cmd=(sbatch --job-name=klique-idle-collector --nodelist="$node" --nodes=1 --ntasks=1
		--cpus-per-task=1 --mem=200M --output=/dev/null --export=ALL,KLIQUE_IDLE_NODE_NAME="$node")
	cmd+=(--partition="$partition")
	[[ -n $time_limit ]] && cmd+=(--time="$time_limit")
	[[ -n $account ]] && cmd+=(--account="$account")
	cmd+=(--wrap "exec $binary")
	if [[ $dry_run -eq 1 ]]; then
		printf '%q ' "${cmd[@]}"
		echo
	elif ! "${cmd[@]}"; then
		echo "warning: submission for $node failed, continuing" >&2
		failed=$((failed + 1))
		continue
	fi
	count=$((count + 1))
done
echo "submitted $count collector job(s); check with: squeue --name=klique-idle-collector"
[[ $failed -eq 0 ]] || die "$failed submission(s) failed"
