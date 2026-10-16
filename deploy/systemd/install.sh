#!/usr/bin/env bash
# Install or upgrade the Klique Idle Collector as a systemd service on one
# node (bare metal, Slurm or PBS). Safe to re-run. Roll out to many nodes with
# your usual tool, e.g.:  pdsh -w gpu[01-64] 'sudo /shared/kic/install.sh ...'
set -euo pipefail

SVC=klique-idle-collector
SVC_USER=klique-idle
BIN_DST=/usr/local/bin/klique-idle-collector
CONF_DIR=/etc/klique-idle-collector
ENV_FILE=$CONF_DIR/collector.env
TOKEN_FILE=$CONF_DIR/token
UNIT_DST=/etc/systemd/system/$SVC.service
DROPIN_DIR=/etc/systemd/system/$SVC.service.d
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

usage() {
	cat <<'EOF'
Usage:
  sudo ./install.sh --cluster NAME --endpoint URL [options]   install or reconfigure
  sudo ./install.sh [--binary PATH]                           upgrade the binary, keep the config
  sudo ./install.sh --uninstall [--purge]                     remove (--purge also removes config and user)

Options:
  --cluster NAME        cluster name shown on the dashboard
  --endpoint URL        where to send reports (https://...)
  --token-file FILE     file containing the bearer token (preferred)
  --token TOKEN         bearer token (visible in shell history and ps)
  --labels k=v,k=v      extra labels, e.g. site=sjc-1,rack=b4
  --env KEY=VALUE       extra collector setting, repeatable, e.g. --env KLIQUE_IDLE_REPORT_INTERVAL=30s
  --binary PATH         collector binary (default: next to this script)
  --run-as-root         run the service as root (needed when /proc is mounted with
                        hidepid, or Slurm hides other users' jobs via PrivateData)
  --no-start            install but don't enable/start the service
EOF
}

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $*"; }

cluster='' endpoint='' token='' token_file='' labels='' binary='' run_as_root=0 start=1 uninstall=0 purge=0
extra_env=()
need_arg() { [[ $# -ge 2 && -n $2 ]] || die "$1 needs a value"; }
while [[ $# -gt 0 ]]; do
	case "$1" in
		--cluster) need_arg "$@"; cluster=$2; shift 2 ;;
		--endpoint) need_arg "$@"; endpoint=$2; shift 2 ;;
		--token) need_arg "$@"; token=$2; shift 2 ;;
		--token-file) need_arg "$@"; token_file=$2; shift 2 ;;
		--labels) need_arg "$@"; labels=$2; shift 2 ;;
		--env) need_arg "$@"; [[ $2 == KLIQUE_IDLE_*=* ]] || die "--env expects KLIQUE_IDLE_<NAME>=value"; extra_env+=("$2"); shift 2 ;;
		--binary) need_arg "$@"; binary=$2; shift 2 ;;
		--run-as-root) run_as_root=1; shift ;;
		--no-start) start=0; shift ;;
		--uninstall) uninstall=1; shift ;;
		--purge) purge=1; shift ;;
		-h | --help) usage; exit 0 ;;
		*) usage >&2; die "unknown option: $1" ;;
	esac
done

[[ $EUID -eq 0 ]] || die "run as root (sudo)"
command -v systemctl >/dev/null || die "systemd is required"

if [[ $uninstall -eq 1 ]]; then
	log "removing $SVC"
	systemctl disable --now "$SVC" 2>/dev/null || true
	rm -rf "$UNIT_DST" "$DROPIN_DIR" "$BIN_DST"
	systemctl daemon-reload
	if [[ $purge -eq 1 ]]; then
		rm -rf "$CONF_DIR"
		userdel "$SVC_USER" 2>/dev/null || true
	fi
	log "done"
	exit 0
fi

# --- binary ------------------------------------------------------------------
if [[ -z $binary ]]; then
	case "$(uname -m)" in
		x86_64) arch=amd64 ;;
		aarch64 | arm64) arch=arm64 ;;
		*) die "unsupported architecture $(uname -m)" ;;
	esac
	for c in "$HERE/klique-idle-collector_linux_$arch" "$HERE/linux_$arch/klique-idle-collector" "$HERE/klique-idle-collector"; do
		if [[ -f $c ]]; then binary=$c; break; fi
	done
	[[ -n $binary ]] || die "collector binary not found next to this script; pass --binary PATH"
fi
[[ -f $binary ]] || die "binary not found: $binary"
version=$("$binary" --version 2>/dev/null) || die "$binary does not run on this machine (wrong architecture?)"

# --- configuration -----------------------------------------------------------
reconfigure=0
if [[ -z $cluster && -z $endpoint && -f $ENV_FILE ]] &&
	[[ -n $token || -n $token_file || -n $labels || ${#extra_env[@]} -gt 0 ]]; then
	die "to change settings pass --cluster and --endpoint too: a reconfigure rewrites the whole configuration"
fi
if [[ -n $cluster || -n $endpoint || ! -f $ENV_FILE ]]; then
	reconfigure=1
	[[ -n $cluster ]] || die "--cluster is required"
	[[ -n $endpoint ]] || die "--endpoint is required"
	[[ $endpoint == http://* || $endpoint == https://* ]] || die "--endpoint must be an http(s) URL"
	[[ -z $token || -z $token_file ]] || die "use only one of --token and --token-file"
	if [[ -n $token_file ]]; then
		[[ -r $token_file ]] || die "cannot read $token_file"
		token=$(tr -d '\r\n' <"$token_file")
	fi
fi

# --- service user --------------------------------------------------------------
if ! id "$SVC_USER" >/dev/null 2>&1; then
	log "creating system user $SVC_USER"
	nologin=$(command -v nologin || echo /sbin/nologin)
	useradd --system --no-create-home --home-dir /nonexistent --shell "$nologin" "$SVC_USER"
fi
# NVIDIA device nodes are normally world read-write; if not, join their group.
if [[ -e /dev/nvidiactl ]]; then
	mode=$(stat -c %a /dev/nvidiactl)
	if [[ ${mode: -1} != 6 && ${mode: -1} != 7 ]]; then
		grp=$(stat -c %G /dev/nvidiactl)
		log "adding $SVC_USER to group $grp for /dev/nvidia* access"
		usermod -aG "$grp" "$SVC_USER"
	fi
fi

# --- files -----------------------------------------------------------------------
log "installing $BIN_DST ($version)"
install -m 0755 "$binary" "$BIN_DST.new"
mv -f "$BIN_DST.new" "$BIN_DST"

quote() { local v=${1//\\/\\\\}; v=${v//\"/\\\"}; printf '"%s"' "$v"; }
if [[ $reconfigure -eq 1 ]]; then
	log "writing $ENV_FILE"
	install -d -m 0750 -o root -g "$SVC_USER" "$CONF_DIR"
	keep_token=0
	if [[ -z $token && -f $TOKEN_FILE ]]; then
		keep_token=1
		log "no token given: keeping the existing $TOKEN_FILE"
	fi
	tmp=$(mktemp "$CONF_DIR/.env.XXXXXX")
	{
		echo "# Klique Idle Collector settings: every flag of 'klique-idle-collector --help'"
		echo "# can be set here as KLIQUE_IDLE_<FLAG>. Restart the service after editing."
		echo "KLIQUE_IDLE_CLUSTER=$(quote "$cluster")"
		echo "KLIQUE_IDLE_ENDPOINT=$(quote "$endpoint")"
		[[ -n $token || $keep_token -eq 1 ]] && echo "KLIQUE_IDLE_TOKEN_FILE=$TOKEN_FILE"
		[[ -n $labels ]] && echo "KLIQUE_IDLE_LABELS=$(quote "$labels")"
		echo "KLIQUE_IDLE_HEALTH_ADDR=127.0.0.1:9478"
		for kv in "${extra_env[@]+"${extra_env[@]}"}"; do echo "${kv%%=*}=$(quote "${kv#*=}")"; done
	} >"$tmp"
	chown root:"$SVC_USER" "$tmp"
	chmod 0640 "$tmp"
	mv -f "$tmp" "$ENV_FILE"
	if [[ -n $token ]]; then
		tmp=$(mktemp "$CONF_DIR/.token.XXXXXX")
		printf '%s\n' "$token" >"$tmp"
		chown root:"$SVC_USER" "$tmp"
		chmod 0640 "$tmp"
		mv -f "$tmp" "$TOKEN_FILE"
	fi
else
	log "keeping existing configuration in $ENV_FILE"
fi

[[ -f $HERE/$SVC.service ]] || die "$SVC.service not found next to this script"
install -m 0644 "$HERE/$SVC.service" "$UNIT_DST"
if [[ $run_as_root -eq 1 ]]; then
	install -d -m 0755 "$DROPIN_DIR"
	# Root alone isn't enough on hidepid=2 hosts: seeing other users' processes
	# needs CAP_SYS_PTRACE, which the unit's empty bounding set would drop.
	printf '[Service]\nUser=root\nGroup=root\nCapabilityBoundingSet=CAP_SYS_PTRACE CAP_DAC_READ_SEARCH\n' >"$DROPIN_DIR/10-run-as-root.conf"
else
	rm -f "$DROPIN_DIR/10-run-as-root.conf"
fi
systemctl daemon-reload

# --- self-test: exactly what one report would contain ---------------------------
run_user=$SVC_USER
[[ $run_as_root -eq 1 ]] && run_user=root
log "self-test: one sample as $run_user (nothing is sent)"
# shellcheck disable=SC1090 # the env file is written above
if ! out=$(set -a && . "$ENV_FILE" && set +a && runuser -u "$run_user" -- "$BIN_DST" --once 2>&1); then
	echo "$out" >&2
	die "self-test failed"
fi
gpus=$(grep -c '"uuid"' <<<"$out" || true)
echo "    GPUs found: $gpus"
grep -o '"name": "NVIDIA[^"]*"' <<<"$out" | sort | uniq -c | sed 's/"name": /    /' || true
grep -o '"kind": "[a-z]*"' <<<"$out" | head -n1 | sed 's/"kind": /    scheduler: /' || true
if grep -q '"errors"' <<<"$out"; then
	echo "    problems reported:"
	sed -n '/"errors": \[/,/\]/p' <<<"$out" | grep -v '"errors"\|\]' | sed 's/^ */      /'
fi

# --- start -----------------------------------------------------------------------
if [[ $start -eq 1 ]]; then
	systemctl enable --quiet "$SVC"
	systemctl restart "$SVC"
	sleep 2
	systemctl is-active --quiet "$SVC" || { journalctl -u "$SVC" -n 20 --no-pager; die "service failed to start"; }
	log "running. Logs: journalctl -u $SVC -f    Health: curl -s localhost:9478/healthz"
else
	log "installed, not started (systemctl enable --now $SVC)"
fi
