#!/usr/bin/env bash
# Runs one sbox job. Reads the run as a tar on stdin (skill/ and sbox/),
# builds its drive, boots Firecracker under the jailer as an unprivileged
# user, streams the serial console to stdout, and removes everything on exit.
#
# Network: each run gets its own network namespace holding only loopback and
# the run's tap device. There is no veth and no route out, so the guest can
# reach the VM side of its tap (172.16.0.1) and nothing else. sbox-dns
# listens there, answers every lookup with NXDOMAIN and logs the names; the
# log is printed after the job, marked with the run's nonce.
#
# Prefetch: if the run carries sbox/prefetch.sh and a prefetch timeout is
# given, a first microVM downloads the skill's dependencies onto an empty
# writable drive. Its only way out is a relay on 172.16.0.1:3128 to a proxy
# that tunnels to the package registries alone. The relay and proxy are
# stopped before the main microVM boots with that drive attached read-only.
#
# Egress: if the run carries sbox/egress (host:port per line), the main
# microVM gets the same relay and proxy, tunnelling to those hosts only.
#
# Sessions: `start` runs the same job in the background and returns once
# the guest is up; the microVM then lives until `stop` or until the guest
# powers off. Each session has a folder under $state/sessions/<id>.
#
# Usage (as root): sbox-run <run-id> <timeout-seconds> [prefetch-timeout-seconds]
#                  sbox-run start <session-id> [project-id]   (run tar on stdin)
#                  sbox-run ps
#                  sbox-run stop <session-id>
#                  sbox-run pause|resume|log <session-id>
#                  sbox-run attach <session-id> [-- <command...>]
#                  sbox-run reconfigure <session-id>  (egress, ports, services on stdin)
#                  sbox-run forget <project-id>    (drop one project's work drive)
#                  sbox-run clean                  (drop every project's)
set -euo pipefail

# Roots can be moved for tests; sudo resets the environment, so callers
# can't move them.
base=${SBOX_BASE:-/opt/sbox}
jail_base=${SBOX_JAIL:-/srv/sbox-jail}
state=${SBOX_STATE:-/var/lib/sbox}
sessions=$state/sessions
egress_entry='^(\*\.)?[a-z0-9.-]+:([0-9]{1,5}|\*)$'
as_vm_user=(setpriv --reuid=sbox-vm --regid=sbox-vm --clear-groups)

# raw_entries prints the egress entries that are raw TCP (an exact host on
# a port other than 443), sorted.
raw_entries() {
	local entry host port
	for entry; do
		host=${entry%:*} port=${entry##*:}
		if [[ $host != \*.* && $port =~ ^[0-9]+$ && $port != 443 ]]; then echo "$entry"; fi
	done | sort
}

# fwd_pair <run> <kind> <port> [host]: a relay and an expose joined by the
# unix socket <run>/fwd/<kind>-<port>.sock, one inside the run's namespace
# and one outside, so the namespace still has no route out. A port goes
# from the guest to the VM's 127.0.0.1, which Lima forwards to the Mac; a
# service goes from 172.16.0.1 (host.sbox in the guest) to the Mac, as
# Lima's host.lima.internal; raw TCP egress goes from 172.16.0.1 to its one
# host. Their pids go to <kind>-<port>.pids, for fwd_stop and teardown.
fwd_pair() {
	local run=$1 kind=$2 port=$3 host=${4:-} netns sock proxy=$base/bin/sbox-proxy.py a b
	netns=sbox-${run##*/} sock=$run/fwd/$kind-$port.sock
	case $kind in
	port)
		ip netns exec "$netns" "${as_vm_user[@]}" python3 "$proxy" expose "$sock" 172.16.0.2 "$port" </dev/null >/dev/null 2>&1 &
		a=$!
		"${as_vm_user[@]}" python3 "$proxy" relay 127.0.0.1 "$port" "$sock" </dev/null >/dev/null 2>&1 &
		b=$!
		;;
	svc | raw)
		[[ $kind == svc ]] && host=host.lima.internal
		ip netns exec "$netns" "${as_vm_user[@]}" python3 "$proxy" relay 172.16.0.1 "$port" "$sock" </dev/null >/dev/null 2>&1 &
		a=$!
		"${as_vm_user[@]}" python3 "$proxy" expose "$sock" "$host" "$port" </dev/null >/dev/null 2>&1 &
		b=$!
		;;
	esac
	echo "$a $b" >"$run/fwd/$kind-$port.pids"
}

# fwd_stop <pids file>: ends a pair fwd_pair started, checking each pid is
# still one of the run's helpers before killing it (pids get reused).
fwd_stop() {
	local pid
	for pid in $(cat "$1" 2>/dev/null); do
		if grep -qF -- "${1%/*}/" "/proc/$pid/cmdline" 2>/dev/null; then kill "$pid" 2>/dev/null || true; fi
	done
	rm -f "$1"
}

# session_ps prints "<id> <state> <started> [project]" per session:
# running or paused while its process lives, exited once it is gone.
session_ps() {
	local dir st project
	for dir in "$sessions"/*/; do
		[[ -f $dir/pid ]] || continue
		st=$(cat "$dir/state")
		kill -0 "$(cat "$dir/pid")" 2>/dev/null || st=exited
		project=""
		[[ -f $dir/project ]] && project=" $(cat "$dir/project")"
		echo "$(basename "$dir") $st $(cat "$dir/started")$project"
	done
}

# teardown removes what a run leaves on the VM side: anything still in its
# namespace, the namespace, its run folder, jails and cgroups. Safe to
# repeat.
teardown() {
	local id=$1 f
	for f in "$state/runs/$id"/fwd/*.pids; do
		[[ -f $f ]] && fwd_stop "$f"
	done
	ip netns pids "sbox-$id" 2>/dev/null | xargs -r kill -KILL 2>/dev/null || true
	ip netns del "sbox-$id" 2>/dev/null || true
	rm -rf "$state/runs/$id" "$jail_base/firecracker/$id" "$jail_base/firecracker/$id-pre"
	rmdir "/sys/fs/cgroup/firecracker/$id" "/sys/fs/cgroup/firecracker/$id-pre" 2>/dev/null || true
}

valid_project() {
	[[ ${1:-} =~ ^[a-f0-9]{16}$ ]] || { echo "sbox-run: bad project id" >&2; exit 2; }
}

# project_free fails when a live session uses the project's work drive.
project_free() {
	local old
	for old in "$sessions"/*/; do
		[[ -f $old/project && $(cat "$old/project") == "$1" ]] || continue
		if kill -0 "$(cat "$old/pid")" 2>/dev/null; then
			echo "sbox-run: project $1 is in use by session $(basename "$old")" >&2
			exit 1
		fi
	done
}

# projects_forget drops the work drives of the given projects, or of all
# of them; none may be in use.
projects_forget() {
	local p
	for p in "$@"; do project_free "$p"; done
	for p in "$@"; do rm -rf "${state:?}/projects/$p"; done
}

valid_id() {
	[[ ${1:-} =~ ^[a-f0-9]{8,32}$ ]] || { echo "sbox-run: bad session id" >&2; exit 2; }
}

# session_dir prints a session's folder, or fails when there is none.
session_dir() {
	valid_id "$1"
	[[ -d $sessions/$1 ]] || { echo "sbox-run: no session $1" >&2; exit 1; }
	echo "$sessions/$1"
}

# session_start keeps the run on disk, runs this script on it in session
# mode in a new process group (so stop can end all of it), and returns once
# the guest's START marker, with the run's nonce, reaches the console log.
# Sessions started more than a day ago are stopped first, so a forgotten
# one doesn't hold memory forever. With a project id, the session gets that
# project's work drive, made on first use (announced with "NEW <id>") and
# kept between sessions.
session_start() {
	local id=${1:-} project=${2:-} dir nonce i old now work="" new=""
	valid_id "$id"
	[[ -z $project ]] || valid_project "$project"
	now=$(date +%s)
	for old in "$sessions"/*/; do
		[[ -f $old/started ]] || continue
		if (($(cat "$old/started") < now - 86400)); then
			session_stop "$(basename "$old")"
		fi
	done
	dir=$sessions/$id
	[[ ! -e $dir ]] || { echo "sbox-run: session $id exists" >&2; exit 1; }
	# One live session per project: two microVMs writing one ext4 drive
	# would corrupt it.
	[[ -z $project ]] || project_free "$project"
	mkdir -p "$dir"
	if [[ -n $project ]]; then
		work=$state/projects/$project/work.ext4
		mkdir -p "${work%/*}"
		if [[ ! -f $work ]]; then
			# Sparse: only what the guest writes takes space.
			truncate -s 20G "$work.tmp"
			mkfs.ext4 -q -L sbox-work -F "$work.tmp"
			mv "$work.tmp" "$work"
			new=1
		fi
		chown sbox-vm "$work"
		echo "$project" >"$dir/project"
	fi
	cat >"$dir/run.tar"
	# A forwarded port already listening in the VM (another session's, most
	# likely) would leave this session's relay unable to bind.
	for port in $(tar -xOf "$dir/run.tar" sbox/ports 2>/dev/null); do
		if [[ -n $(ss -Htln "sport = :$port") ]]; then
			echo "sbox-run: port $port is already in use in the VM (sbox ps: another session?)" >&2
			rm -rf "$dir"
			exit 1
		fi
	done
	nonce=$(tar -xOf "$dir/run.tar" sbox/nonce)
	date +%s >"$dir/started"
	echo running >"$dir/state"
	SBOX_SESSION=1 SBOX_WORK=$work setsid "${BASH:-bash}" "$0" "$id" 0 <"$dir/run.tar" >"$dir/console.log" 2>&1 &
	echo $! >"$dir/pid"
	for ((i = 0; i < 1200; i++)); do # 120 s
		if grep -q "SBOX-$nonce START" "$dir/console.log"; then
			rm -f "$dir/run.tar"
			[[ -z $new ]] || echo "NEW $id"
			echo "READY $id"
			return 0
		fi
		kill -0 "$(cat "$dir/pid")" 2>/dev/null || break
		sleep 0.1
	done
	echo "sbox-run: session $id did not start" >&2
	tail -n 20 "$dir/console.log" >&2
	kill -KILL -- "-$(cat "$dir/pid")" 2>/dev/null || true
	rm -rf "$dir"
	return 1
}

# vm_api sets a session's microVM state (Paused, Resumed) through
# Firecracker's API.
vm_api() {
	curl -fsS --unix-socket "$jail_base/firecracker/$1/root/run/firecracker.socket" \
		-X PATCH -H 'Content-Type: application/json' -d "{\"state\":\"$2\"}" http://localhost/vm
}

# session_stop ends a session's process group: TERM first, so the run's own
# cleanup stops its helpers; KILL after 10 s. The teardown then runs here
# too, in case that cleanup never got to. Before that, the guest flushes
# its writes to the work drive (a paused one is resumed for it), or the
# last seconds of work would be lost with the guest's page cache.
session_stop() {
	local id=${1:-} dir pid i
	dir=$(session_dir "$id")
	pid=$(cat "$dir/pid")
	if kill -0 "$pid" 2>/dev/null; then
		if [[ $(cat "$dir/state") == paused ]]; then
			vm_api "$id" Resumed && echo running >"$dir/state" || true
		fi
		if [[ -f $dir/root_key ]]; then
			ip netns exec "sbox-$id" timeout 10 ssh -i "$dir/root_key" -o UserKnownHostsFile="$dir/known_hosts" \
				-o HostKeyAlias=sbox-guest -o StrictHostKeyChecking=yes -o BatchMode=yes -o LogLevel=ERROR \
				root@172.16.0.2 </dev/null >/dev/null 2>&1 || true
		fi
		kill -TERM -- "-$pid" 2>/dev/null || true
		for ((i = 0; i < 100; i++)); do
			kill -0 "$pid" 2>/dev/null || break
			sleep 0.1
		done
		kill -KILL -- "-$pid" 2>/dev/null || true
	fi
	teardown "$id"
	rm -rf "$dir"
}

# session_vm_state moves a live session from one state to another through
# Firecracker's API (PATCH /vm), and records the new state only once
# Firecracker has accepted it.
session_vm_state() {
	local id=$1 from=$2 to=$3 vm_state=$4 dir
	dir=$(session_dir "$id")
	if ! kill -0 "$(cat "$dir/pid")" 2>/dev/null || [[ $(cat "$dir/state") != "$from" ]]; then
		echo "sbox-run: session $id is not $from" >&2
		exit 1
	fi
	vm_api "$id" "$vm_state" || { echo "sbox-run: Firecracker did not change session $id to $to" >&2; exit 1; }
	echo "$to" >"$dir/state"
}

# session_attach opens SSH into a running session's guest from inside its
# namespace, with the guest's host key pinned. The command (by default a
# login shell) starts in the project folder; a terminal on stdin gets one
# in the guest too.
session_events() {
	sed 's/^/RULE /' "$state/runs/$1/drive/sbox/egress" 2>/dev/null || true
	sed 's/^/PORT /' "$state/runs/$1/drive/sbox/ports" 2>/dev/null || true
	sed 's/^/EGRESS /' "$state/runs/$1/egress.log" 2>/dev/null || true
	sed 's/^/DNS /' "$state/runs/$1/dns.log" 2>/dev/null || true
	ip netns exec "sbox-$1" nft list set ip sbox refused 2>/dev/null | grep -oE '[0-9.]+ \. [0-9]+' | sed 's/ \. /:/; s/^/REFUSED /' || true
}

# redacted <id> <command...>: runs the command with the session's secrets
# (and their hex and base64 forms) taken out of its output, since the
# guest can echo one into a lookup or its console.
redacted() {
	local secrets=$state/runs/$1/drive/sbox/secrets
	shift
	if [[ -s $secrets ]]; then
		"$@" | python3 -I "$base/bin/sbox-redact.py" "$secrets"
	else
		"$@"
	fi
}

session_attach() {
	local id=${1:-} dir cmd="bash -l" tty=()
	dir=$(session_dir "$id")
	shift
	if [[ ${1:-} == -- ]]; then shift; fi
	if ! kill -0 "$(cat "$dir/pid")" 2>/dev/null || [[ $(cat "$dir/state") != running ]]; then
		echo "sbox-run: session $id is not running" >&2
		exit 1
	fi
	if (($#)); then
		cmd=$(printf '%q ' "$@")
		cmd=${cmd% }
	fi
	if [[ -t 0 ]]; then tty=(-t); fi
	exec ip netns exec "sbox-$id" ssh ${tty[@]+"${tty[@]}"} -i "$dir/key" \
		-o UserKnownHostsFile="$dir/known_hosts" -o HostKeyAlias=sbox-guest -o StrictHostKeyChecking=yes \
		-o ConnectionAttempts=10 -o LogLevel=ERROR runner@172.16.0.2 "cd /home/runner/work && exec $cmd"
}

# session_reconfigure takes a running session's egress, ports and services
# on stdin ("egress <host:port>", "port <n>", "service <n>", every line of
# each), checks them all, then applies what changed: the proxy rereads its
# rules file, port and service pairs stop and start, and the namespace's
# exemptions from the refused-connection set follow the services. Raw TCP
# egress, and egress for a session that started without any, take a new
# session.
session_reconfigure() {
	local id=${1:-} dir run kind value p old=() egress=() ports=() services=()
	dir=$(session_dir "$id")
	run=$state/runs/$id
	if ! kill -0 "$(cat "$dir/pid")" 2>/dev/null; then
		echo "sbox-run: session $id is not running" >&2
		exit 1
	fi
	while read -r kind value; do
		case $kind in
		egress)
			[[ $value =~ $egress_entry ]] || { echo "sbox-run: bad egress entry" >&2; exit 1; }
			egress+=("$value")
			;;
		port | service)
			if ! [[ $value =~ ^[0-9]{1,5}$ ]] || ((value < 1 || value > 65535)); then
				echo "sbox-run: bad port" >&2
				exit 1
			fi
			if [[ $kind == port ]]; then ports+=("$value"); else services+=("$value"); fi
			;;
		*) echo "sbox-run: bad line" >&2; exit 1 ;;
		esac
	done
	while read -r value; do [[ -n $value ]] && old+=("$value"); done <"$run/drive/sbox/egress" 2>/dev/null || true
	if [[ $(raw_entries ${egress[@]+"${egress[@]}"}) != "$(raw_entries ${old[@]+"${old[@]}"})" ]]; then
		echo "sbox-run: raw TCP egress (a port other than 443) changed: a new session applies it" >&2
		exit 1
	fi
	if ((${#egress[@]})) && [[ ! -f $run/egress.rules ]]; then
		echo "sbox-run: session $id started without egress: a new session applies it" >&2
		exit 1
	fi
	for p in ${ports[@]+"${ports[@]}"}; do
		grep -qx "$p" "$run/drive/sbox/ports" 2>/dev/null && continue
		if [[ -n $(ss -Htln "sport = :$p") ]]; then
			echo "sbox-run: port $p is already in use in the VM (sbox ps: another session?)" >&2
			exit 1
		fi
	done

	if [[ -f $run/egress.rules ]]; then
		printf '%s\n' ${egress[@]+"${egress[@]}"} | grep . >"$run/egress.rules.new" || true
		mv "$run/egress.rules.new" "$run/egress.rules"
		cp "$run/egress.rules" "$run/drive/sbox/egress"
	fi
	install -d -o sbox-vm -m 0700 "$run/fwd"
	for p in $(cat "$run/drive/sbox/ports" 2>/dev/null); do
		[[ " ${ports[*]-} " == *" $p "* ]] || fwd_stop "$run/fwd/port-$p.pids"
	done
	for p in ${ports[@]+"${ports[@]}"}; do
		grep -qx "$p" "$run/drive/sbox/ports" 2>/dev/null || fwd_pair "$run" port "$p"
	done
	for p in $(cat "$run/drive/sbox/services" 2>/dev/null); do
		if [[ " ${services[*]-} " != *" $p "* ]]; then
			fwd_stop "$run/fwd/svc-$p.pids"
			ip netns exec "sbox-$id" nft delete element ip sbox listening "{ 172.16.0.1 . $p }" 2>/dev/null || true
		fi
	done
	for p in ${services[@]+"${services[@]}"}; do
		if ! grep -qx "$p" "$run/drive/sbox/services" 2>/dev/null; then
			fwd_pair "$run" svc "$p"
			ip netns exec "sbox-$id" nft add element ip sbox listening "{ 172.16.0.1 . $p }" 2>/dev/null || true
		fi
	done
	printf '%s\n' ${ports[@]+"${ports[@]}"} | grep . >"$run/drive/sbox/ports" || true
	printf '%s\n' ${services[@]+"${services[@]}"} | grep . >"$run/drive/sbox/services" || true
}

case ${1:-} in
ps) session_ps; exit 0 ;;
forget) valid_project "${2:-}"; projects_forget "$2"; exit 0 ;;
clean)
	all=()
	for p in "$state/projects"/*/; do [[ -d $p ]] && all+=("$(basename "$p")"); done
	((${#all[@]} == 0)) || projects_forget "${all[@]}"
	exit 0
	;;
attach) shift; session_attach "$@" ;;
reconfigure) session_reconfigure "${2:-}"; exit 0 ;;
log) dir=$(session_dir "${2:-}"); redacted "$2" cat "$dir/console.log"; exit 0 ;;
events) # the allowlist and forwarded ports, then what the proxy and DNS logger saw
	session_dir "${2:-}" >/dev/null
	redacted "$2" session_events "$2"
	exit 0
	;;
pause) session_vm_state "${2:-}" running paused Paused; exit 0 ;;
resume) session_vm_state "${2:-}" paused running Resumed; exit 0 ;;
stop) shift; session_stop "$@"; exit 0 ;;
start) shift; session_start "$@"; exit 0 ;;
esac

id=${1:-} timeout=${2:-} prefetch_timeout=${3:-0}
[[ $id =~ ^[a-f0-9]{8,32}$ ]] || { echo "sbox-run: bad run id" >&2; exit 2; }
[[ $timeout =~ ^[0-9]{1,5}$ ]] || { echo "sbox-run: bad timeout" >&2; exit 2; }
[[ $prefetch_timeout =~ ^[0-9]{1,5}$ ]] || { echo "sbox-run: bad prefetch timeout" >&2; exit 2; }

run=$state/runs/$id
netns=sbox-$id

pids=()
cleanup() {
	# Some are already stopped; a failed kill must not abort the cleanup.
	((${#pids[@]})) && kill "${pids[@]}" 2>/dev/null || true
	# Anything else still in the run's namespace goes too, so nothing a run
	# starts outlives it.
	teardown "$id"
}
trap cleanup EXIT

mkdir -p "$run/drive"
tar -x -C "$run/drive" --no-same-owner --no-same-permissions
nonce=$(cat "$run/drive/sbox/nonce")

# A session's guest runs sshd. The keys are made here: the client's private
# key stays root-only in the session folder, the guest gets its public key
# and a host key, and that host key is pinned in the session's known_hosts.
if [[ ${SBOX_SESSION:-} == 1 ]]; then
	sdir=$sessions/$id
	mkdir -p "$sdir" "$run/drive/sbox/ssh"
	rm -f "$sdir/key" "$sdir/key.pub" "$sdir/root_key" "$sdir/root_key.pub"
	ssh-keygen -q -t ed25519 -N '' -C sbox -f "$sdir/key"
	ssh-keygen -q -t ed25519 -N '' -C sbox-flush -f "$sdir/root_key"
	ssh-keygen -q -t ed25519 -N '' -C sbox-guest -f "$run/drive/sbox/ssh/host_ed25519"
	cp "$sdir/key.pub" "$run/drive/sbox/ssh/authorized_keys.runner"
	# Root's key can only run sync: stop uses it to flush the work drive.
	echo "command=\"sync\",no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding $(cat "$sdir/root_key.pub")" \
		>"$run/drive/sbox/ssh/authorized_keys.root"
	echo "sbox-guest $(cat "$run/drive/sbox/ssh/host_ed25519.pub")" >"$sdir/known_hosts"
fi

# The run's drive: the skill plus the guest init and job, read-only in the guest.
size_mb=$(($(du -sm "$run/drive" | cut -f1) * 13 / 10 + 32))
truncate -s "${size_mb}M" "$run/drive.ext4"
mkfs.ext4 -q -d "$run/drive" -F "$run/drive.ext4"

ip netns add "$netns"
in_ns() { ip netns exec "$netns" "$@"; }
in_ns ip link set lo up
in_ns ip tuntap add dev tap0 mode tap user "$(id -u sbox-vm)"
in_ns ip addr add 172.16.0.1/30 dev tap0
in_ns ip link set tap0 up

# Helpers run unprivileged (as_vm_user). They are started directly, not via
# in_ns: ip netns exec and setpriv exec the next program, so $! is the
# helper itself. Their stdio is detached, or they would hold this script's
# stdout open.

# Egress: host:port entries, the host a name or *.domain, the port a number
# or *. An exact host on a port other than 443 is raw TCP: the DNS logger
# answers its name with 172.16.0.1, where a relay carries the connection to
# that host alone. The rest go through the HTTPS proxy.
egress=() raw=()
if [[ -s $run/drive/sbox/egress ]]; then
	while read -r entry; do
		[[ $entry =~ $egress_entry ]] || continue
		egress+=("$entry")
		host=${entry%:*} port=${entry##*:}
		if [[ $host != \*.* && $port =~ ^[0-9]+$ && $port != 443 ]]; then raw+=("$entry"); fi
	done <"$run/drive/sbox/egress"
fi
raw_hosts=()
for entry in ${raw[@]+"${raw[@]}"}; do raw_hosts+=("${entry%:*}"); done

# Unprivileged binds on any port, in this namespace only: the DNS logger
# (53), and relays for raw TCP egress (22, say) and services. Only sbox's
# helpers listen here; the guest is on the other side of the tap.
in_ns sysctl -qw net.ipv4.ip_unprivileged_port_start=0
install -o sbox-vm -m 0600 /dev/null "$run/dns.log"
ip netns exec "$netns" "${as_vm_user[@]}" \
	python3 "$base/bin/sbox-dns.py" 172.16.0.1 53 "$run/dns.log" ${raw_hosts[@]+"${raw_hosts[@]}"} </dev/null >/dev/null 2>&1 &
pids+=($!)

# Refused connections: the guest's TCP attempts at anything but sbox's own
# listeners here (DNS, the proxy relay, raw TCP and service relays) land in
# an nft set that sbox-run events reads. It only records: the namespace has
# no route out either way.
listening="172.16.0.1 . 53, 172.16.0.1 . 3128"
for entry in ${raw[@]+"${raw[@]}"}; do listening+=", 172.16.0.1 . ${entry##*:}"; done
for port in $(cat "$run/drive/sbox/services" 2>/dev/null); do listening+=", 172.16.0.1 . $port"; done
in_ns nft -f - <<EOF || echo "sbox-run: refused connections won't be listed (nft failed)" >&2
table ip sbox {
	set refused { type ipv4_addr . inet_service; flags dynamic; size 4096; }
	set listening { type ipv4_addr . inet_service; elements = { $listening }; }
	chain prerouting {
		type filter hook prerouting priority 0;
		iifname "tap0" tcp flags & (syn | ack) == syn ip daddr . tcp dport != @listening add @refused { ip daddr . tcp dport }
	}
}
EOF

# boot <jail id> <timeout> <phase> <deps drive mode: none|ro|rw>
# Boots one microVM and returns the jailer's status (124/137: timed out).
boot() {
	local jid=$1 secs=$2 phase=$3 deps=$4
	local root=$jail_base/firecracker/$jid/root
	mkdir -p "$root"
	# Same filesystem, so hard links: no copy of the kernel, the 800 MB
	# rootfs or the dependency drive.
	ln "$base/vmlinux" "$root/vmlinux"
	ln "$base/rootfs.ext4" "$root/rootfs.ext4"
	ln "$run/drive.ext4" "$root/drive.ext4"
	local extra=""
	if [[ $deps != none ]]; then
		ln "$run/deps.ext4" "$root/deps.ext4"
		local ro=true
		[[ $deps == rw ]] && ro=false
		extra=", {\"drive_id\": \"deps\", \"path_on_host\": \"/deps.ext4\", \"is_root_device\": false, \"is_read_only\": $ro}"
	fi
	# A session's project work drive, writable; the guest finds it by label.
	if [[ -n ${SBOX_WORK:-} && $phase == main ]]; then
		ln "$SBOX_WORK" "$root/work.ext4"
		extra+=", {\"drive_id\": \"work\", \"path_on_host\": \"/work.ext4\", \"is_root_device\": false, \"is_read_only\": false}"
	fi
	# sbox/vm: "<vcpus> <MiB>" for the main microVM; 0 or anything else
	# keeps the default.
	local vcpus=2 mem=1024 v m
	if [[ $phase == main && -s $run/drive/sbox/vm ]]; then
		read -r v m <"$run/drive/sbox/vm"
		[[ $v =~ ^[1-9][0-9]?$ ]] && vcpus=$v
		[[ $m =~ ^[1-9][0-9]{2,5}$ ]] && mem=$m
	fi
	cat >"$root/vm.json" <<EOF
{
  "boot-source": {
    "kernel_image_path": "/vmlinux",
    "boot_args": "console=ttyS0 reboot=k panic=1 quiet loglevel=1 ro init=/sbin/sbox-init sbox.phase=$phase"
  },
  "drives": [
    {"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": true},
    {"drive_id": "run", "path_on_host": "/drive.ext4", "is_root_device": false, "is_read_only": true}$extra
  ],
  "network-interfaces": [{"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "06:00:ac:10:00:02"}],
  "machine-config": {"vcpu_count": $vcpus, "mem_size_mib": $mem}
}
EOF
	# Sessions are paused and resumed through Firecracker's API; one-shot
	# runs need no API.
	local api=(--no-api)
	if [[ ${SBOX_SESSION:-} == 1 && $phase == main ]]; then
		mkdir -p "$root/run"
		chown sbox-vm "$root/run"
		api=(--api-sock /run/firecracker.socket)
	fi
	local status=0
	# </dev/null: the serial console reads stdin.
	timeout --kill-after=5 "$secs" "$base/bin/jailer" \
		--id "$jid" \
		--exec-file "$base/bin/firecracker" \
		--uid "$(id -u sbox-vm)" --gid "$(id -g sbox-vm)" \
		--chroot-base-dir "$jail_base" \
		--cgroup-version 2 \
		--netns "/var/run/netns/$netns" \
		-- "${api[@]}" --config-file /vm.json </dev/null || status=$?
	return "$status"
}

# start_egress <log> [host:port ...]: an allowlist proxy in the VM's main
# namespace on a unix socket (default allowlist: the package registries),
# and a relay to it on 172.16.0.1:3128 inside the run's namespace.
start_egress() {
	local log=$1
	shift
	install -d -o sbox-vm -m 0700 "$run/proxy"
	rm -f "$run/proxy/sock"
	install -o sbox-vm -m 0600 /dev/null "$log"
	"${as_vm_user[@]}" python3 "$base/bin/sbox-proxy.py" serve "$run/proxy/sock" "$log" "$@" </dev/null >/dev/null 2>&1 &
	proxy_pid=$!
	for _ in $(seq 50); do [[ -S $run/proxy/sock ]] && break; sleep 0.1; done
	ip netns exec "$netns" "${as_vm_user[@]}" \
		python3 "$base/bin/sbox-proxy.py" relay 172.16.0.1 3128 "$run/proxy/sock" </dev/null >/dev/null 2>&1 &
	relay_pid=$!
	pids+=("$proxy_pid" "$relay_pid")
}

stop_egress() {
	kill "$relay_pid" "$proxy_pid" 2>/dev/null || true
	wait "$relay_pid" "$proxy_pid" 2>/dev/null || true
}

deps=none
if [[ -f $run/drive/sbox/prefetch.sh && $prefetch_timeout -gt 0 ]]; then
	truncate -s 4G "$run/deps.ext4"
	mkfs.ext4 -q -F "$run/deps.ext4"
	chown sbox-vm "$run/deps.ext4"

	start_egress "$run/prefetch.log"
	pre_status=0
	boot "$id-pre" "$prefetch_timeout" prefetch rw || pre_status=$?
	stop_egress # no way out for the main microVM, unless it declares egress

	[[ $pre_status == 124 || $pre_status == 137 ]] && echo "SBOX-$nonce PREFETCH-TIMEOUT"
	sed "s/^/SBOX-$nonce FETCH /" "$run/prefetch.log"
	deps=ro
fi

# The main microVM's proxy reads its allowlist from a file, so reconfigure
# can change it while tunnels stay open.
if ((${#egress[@]})); then
	printf '%s\n' "${egress[@]}" >"$run/egress.rules"
	start_egress "$run/egress.log" --rules "$run/egress.rules"
fi

# Forwarded ports, services and raw TCP egress: a pair each (fwd_pair).
if [[ -s $run/drive/sbox/ports || -s $run/drive/sbox/services || ${#raw[@]} -gt 0 ]]; then
	install -d -o sbox-vm -m 0700 "$run/fwd"
fi
for entry in ${raw[@]+"${raw[@]}"}; do
	fwd_pair "$run" raw "${entry##*:}" "${entry%:*}"
done
for port in $(cat "$run/drive/sbox/ports" 2>/dev/null); do
	[[ $port =~ ^[0-9]{1,5}$ ]] && fwd_pair "$run" port "$port"
done
for port in $(cat "$run/drive/sbox/services" 2>/dev/null); do
	[[ $port =~ ^[0-9]{1,5}$ ]] && fwd_pair "$run" svc "$port"
done

# dev.pause_after: pause the session's microVM once nothing has been
# connected to the guest (a terminal, a forwarded port) for that many
# seconds. sbox dev resumes it when attaching again.
idle_pause() {
	local limit=$1 dir=$sessions/$id busy now
	busy=$(date +%s)
	while sleep "${SBOX_IDLE_POLL:-$((limit < 30 ? limit : 30))}"; do
		now=$(date +%s)
		if [[ $(cat "$dir/state" 2>/dev/null) != running ]] ||
			[[ -n $(ip netns exec "$netns" ss -Htn state established dst 172.16.0.2 2>/dev/null) ]]; then
			busy=$now
		elif ((now - busy >= limit)) && vm_api "$id" Paused >/dev/null; then
			echo paused >"$dir/state"
		fi
	done
}
pause_after=$(cat "$run/drive/sbox/pause_after" 2>/dev/null || true)
if [[ ${SBOX_SESSION:-} == 1 && $pause_after =~ ^[1-9][0-9]{0,6}$ ]]; then
	idle_pause "$pause_after" </dev/null >/dev/null 2>&1 &
	pids+=($!)
fi

# Exit 124 on timeout, 0 otherwise: the console (stdout) belongs to the
# guest, so the timeout is reported out of its reach.
status=0
boot "$id" "$timeout" main "$deps" || status=$?

if ((${#egress[@]})); then
	stop_egress
	sed "s/^/SBOX-$nonce EGRESS /" "$run/egress.log"
fi

# The guests are gone, so they can't interleave forged lines.
sed "s/^/SBOX-$nonce DNS /" "$run/dns.log"

if [[ $status == 124 || $status == 137 ]]; then
	exit 124
fi
exit 0
