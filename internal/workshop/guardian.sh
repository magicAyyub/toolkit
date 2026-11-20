#!/bin/bash
# Workshop guardian. Runs inside the pod, so it keeps working while the laptop
# is asleep or offline, and deletes the pod when:
#   - the budget deadline (WS_DEADLINE) is reached, or
#   - the machine is idle: no SSH session AND every GPU under the busy
#     threshold, continuously for WS_IDLE_SECONDS. Any busy sample resets the
#     idle clock, so a GPU job left running after disconnecting keeps the pod.
#
# It deletes the pod with the RUNPOD_API_KEY that RunPod injects into every
# pod. That key is scoped to this pod: it can read and delete it, nothing else.
#
# The WS_POLL_SECONDS, WS_STATE_DIR, WS_TTY_DIR and WS_API_URL overrides exist
# for tests.

poll_seconds=${WS_POLL_SECONDS:-30}
gpu_busy_percent=5
state_dir=${WS_STATE_DIR:-/etc/workshop}
tty_dir=${WS_TTY_DIR:-/dev/pts}
api_url=${WS_API_URL:-https://api.runpod.io}

log() {
	echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) $*"
}

# Write a message on every open terminal. wall depends on utmp, which is not
# reliably maintained inside containers, so write to the ptys directly.
broadcast() {
	local tty
	for tty in "$tty_dir"/[0-9]*; do
		if [ -c "$tty" ] || [ -f "$tty" ]; then
			printf '\r\n\033[1;33m[workshop] %s\033[0m\r\n' "$1" >>"$tty" 2>/dev/null
		fi
	done
}

ssh_session_count() {
	# Each interactive or exec SSH session has a privilege-separated child named
	# "sshd: user@pts/N" or "sshd: user@notty" ("sshd-session: ..." on OpenSSH >= 9.8).
	local count
	count=$(pgrep -fc 'sshd(-session)?: [^ ]+@')
	echo "${count:-0}"
}

# Succeeds only when every GPU reports a utilization under the threshold. If
# the GPUs can't be read, they are treated as busy: never delete on a guess.
gpu_at_rest() {
	local utilizations u
	utilizations=$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits 2>/dev/null) || return 1
	[ -n "$utilizations" ] || return 1
	for u in $utilizations; do
		case "$u" in
		'' | *[!0-9]*) return 1 ;;
		esac
		[ "$u" -lt "$gpu_busy_percent" ] || return 1
	done
	return 0
}

delete_pod() {
	log "deleting pod $RUNPOD_POD_ID: $1"
	local status
	status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 -X DELETE \
		-H "Authorization: Bearer $RUNPOD_API_KEY" \
		"$api_url/v2/pods/$RUNPOD_POD_ID")
	log "delete returned HTTP $status"
	if [ "$status" = "204" ] || [ "$status" = "404" ]; then
		exit 0
	fi
	# Otherwise keep looping: the next poll retries the deletion.
}

install_prompt() {
	mkdir -p "$state_dir"
	cat >"$state_dir/workshop.env" <<EOF
WS_NAME=$WS_NAME
WS_CREATED=$WS_CREATED
WS_DEADLINE=$WS_DEADLINE
WS_PRICE_PER_HOUR=$WS_PRICE_PER_HOUR
WS_BUDGET=$WS_BUDGET
WS_IDLE_SECONDS=$WS_IDLE_SECONDS
EOF

	cat >"$state_dir/prompt.sh" <<'EOF'
# Installed by the workshop guardian: shows elapsed time and estimated cost in
# the prompt, and provides the ws-status command.
. "$(dirname "${BASH_SOURCE[0]}")/workshop.env"

__ws_summary() {
	local now elapsed limit
	now=$(date +%s)
	elapsed=$((now - WS_CREATED))
	limit=$((WS_DEADLINE - WS_CREATED))
	awk -v e="$elapsed" -v l="$limit" -v p="$WS_PRICE_PER_HOUR" 'BEGIN {
		printf "%dh%02d/%dh%02d | ~$%.2f/$%.2f est.", int(e/3600), int(e%3600/60), int(l/3600), int(l%3600/60), e*p/3600, l*p/3600
	}'
}

ws-status() {
	local remaining=$((WS_DEADLINE - $(date +%s)))
	echo "Workshop:   $WS_NAME"
	echo "Usage:      $(__ws_summary)"
	echo "Price:      \$$WS_PRICE_PER_HOUR/h"
	echo "Budget:     $WS_BUDGET, deleted in $((remaining / 60)) min (at $(date -d "@$WS_DEADLINE" '+%H:%M %Z'))"
	echo "Idle rule:  deleted after $((WS_IDLE_SECONDS / 60)) min with no SSH session and GPU at rest"
}

PS1='[ws $(__ws_summary)] '"$PS1"
EOF

	local bashrc="${HOME:-/root}/.bashrc"
	local source_line="[ -f $state_dir/prompt.sh ] && . $state_dir/prompt.sh"
	if ! grep -qF "$source_line" "$bashrc" 2>/dev/null; then
		echo "$source_line" >>"$bashrc"
	fi
}

if [ -z "$RUNPOD_API_KEY" ] || [ -z "$RUNPOD_POD_ID" ]; then
	log "WARNING: RUNPOD_API_KEY or RUNPOD_POD_ID missing: this pod cannot delete itself"
fi

install_prompt
log "guardian started for workshop $WS_NAME: deadline $WS_DEADLINE, idle timeout ${WS_IDLE_SECONDS}s"

warned_10_minutes=no
warned_2_minutes=no
idle_since=""

while true; do
	now=$(date +%s)

	remaining=$((WS_DEADLINE - now))
	minutes_left=$(((remaining + 59) / 60))
	if [ "$remaining" -le 0 ]; then
		broadcast "Budget reached ($WS_BUDGET). Deleting this machine now."
		delete_pod "budget reached"
	elif [ "$remaining" -le 120 ] && [ "$warned_2_minutes" = no ]; then
		broadcast "Budget almost exhausted: this machine will be DELETED in $minutes_left min. Everything on it will be lost."
		warned_2_minutes=yes
		warned_10_minutes=yes
	elif [ "$remaining" -le 600 ] && [ "$warned_10_minutes" = no ]; then
		broadcast "Budget almost exhausted: this machine will be DELETED in $minutes_left min. Everything on it will be lost."
		warned_10_minutes=yes
	fi

	if [ "$(ssh_session_count)" -eq 0 ] && gpu_at_rest; then
		if [ -z "$idle_since" ]; then
			idle_since=$now
			log "idle: no SSH session and GPU at rest"
		fi
		if [ $((now - idle_since)) -ge "$WS_IDLE_SECONDS" ]; then
			delete_pod "idle for $((now - idle_since))s"
		fi
	elif [ -n "$idle_since" ]; then
		log "active again after $((now - idle_since))s idle"
		idle_since=""
	fi

	sleep "$poll_seconds"
done
