#!/usr/bin/env bash
# DRAFT, untested on a live PBS cluster. Fallback for trying the collector on
# OpenPBS / PBS Professional without root: submits one long-running job per
# node. Read deploy/pbs/README.md first. The recommended deployment is the
# systemd service on every node (deploy/systemd/install.sh).
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: submit-collectors.sh --binary PATH --cluster NAME --endpoint URL [options]

  --binary PATH       collector binary on a filesystem every node can read
  --cluster NAME      cluster name shown on the dashboard
  --endpoint URL      where to send reports
  --token-file FILE   token file readable on every node (shared filesystem)
  --queue NAME        queue to submit to (default: the server's default queue)
  --nodes "N1 N2"     nodes to cover (default: every node pbsnodes lists)
  --walltime HH:MM:SS job time limit (default: the queue's maximum)
  --dry-run           print the qsub commands instead of running them

Stop them with: qdel $(qselect -N klique-idle-coll -u $USER)
EOF
}

die() { echo "error: $*" >&2; exit 1; }

binary='' cluster='' endpoint='' token_file='' queue='' nodes='' walltime='' dry_run=0
need_arg() { [[ $# -ge 2 && -n $2 ]] || die "$1 needs a value"; }
while [[ $# -gt 0 ]]; do
	case "$1" in
		--binary) need_arg "$@"; binary=$2; shift 2 ;;
		--cluster) need_arg "$@"; cluster=$2; shift 2 ;;
		--endpoint) need_arg "$@"; endpoint=$2; shift 2 ;;
		--token-file) need_arg "$@"; token_file=$2; shift 2 ;;
		--queue) need_arg "$@"; queue=$2; shift 2 ;;
		--nodes) need_arg "$@"; nodes=$2; shift 2 ;;
		--walltime) need_arg "$@"; walltime=$2; shift 2 ;;
		--dry-run) dry_run=1; shift ;;
		-h | --help) usage; exit 0 ;;
		*) usage >&2; die "unknown option: $1" ;;
	esac
done
[[ -n $binary && -n $cluster && -n $endpoint ]] || { usage >&2; die "--binary, --cluster and --endpoint are required"; }
[[ -x $binary ]] || die "$binary is not executable"
[[ -z $token_file || -r $token_file ]] || die "cannot read $token_file"
command -v qsub >/dev/null || die "qsub not found: run this on a PBS login node"

if [[ -z $nodes ]]; then
	# pbsnodes -a prints each node name at the start of a line, attributes indented.
	nodes=$(pbsnodes -a | awk '/^[^ \t]/ {print $1}' | paste -sd' ' -)
	[[ -n $nodes ]] || die "pbsnodes listed no nodes"
fi

export KLIQUE_IDLE_CLUSTER=$cluster KLIQUE_IDLE_ENDPOINT=$endpoint KLIQUE_IDLE_SCHEDULER=pbs KLIQUE_IDLE_HEALTH_ADDR=off
[[ -n $token_file ]] && export KLIQUE_IDLE_TOKEN_FILE=$token_file

count=0
for node in $nodes; do
	# -V passes the KLIQUE_IDLE_* settings above; job names are cut to 15 chars by PBS.
	cmd=(qsub -N klique-idle-coll -V -v "KLIQUE_IDLE_NODE_NAME=$node"
		-l "select=1:host=$node:ncpus=1:mem=200mb" -j oe -o /dev/null)
	[[ -n $queue ]] && cmd+=(-q "$queue")
	[[ -n $walltime ]] && cmd+=(-l "walltime=$walltime")
	cmd+=(-- "$binary")
	if [[ $dry_run -eq 1 ]]; then
		printf '%q ' "${cmd[@]}"
		echo
	else
		"${cmd[@]}"
	fi
	count=$((count + 1))
done
echo "submitted $count collector job(s); check with: qstat -u \$USER"
