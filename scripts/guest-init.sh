#!/bin/sh
# Per-run guest init, started by /sbin/sbox-init from the run's drive.
# The root filesystem is read-only: writable scratch lives in tmpfs. The
# command runs as the unprivileged runner user, then the VM powers off.
# Markers carry the run's nonce, readable by root only, so the command
# can't print a fake result.
nonce=$(cat /drive/sbox/nonce)
canary=$(cat /drive/sbox/canary)
phase=main
grep -q 'sbox.phase=prefetch' /proc/cmdline && phase=prefetch

poweroff() {
	sync
	# No systemd, so no reboot(8): ask the kernel directly. Firecracker exits on reboot.
	echo 1 >/proc/sys/kernel/sysrq
	echo b >/proc/sysrq-trigger
}
# Cap the runner's processes (threads count too), so a fork bomb or a
# runaway build fails inside the guest. Everything started from here
# inherits it; root, so sshd, is exempt and still lets you in.
prlimit --pid $$ --nproc=2048:2048
for d in /tmp /var/tmp /run; do
	mount -t tmpfs -o mode=1777,size=256m tmpfs "$d"
done
# A session on a project brings that project's work drive, the third:
# the runner's home, kept between sessions. Otherwise the home is scratch,
# like /tmp.
kept_home=
if [ -d /drive/sbox/ssh ] && mount -t ext4 -o noatime /dev/vdc /home/runner 2>/dev/null; then
	kept_home=1
else
	mount -t tmpfs -o mode=1777,size=256m tmpfs /home/runner
fi
chmod 0755 /home/runner
# Pseudo-terminals, for ssh sessions and programs that want one (normally
# mounted by systemd).
mkdir -p /dev/pts
mount -t devpts -o gid=5,mode=0620,ptmxmode=0666 devpts /dev/pts
[ -e /dev/ptmx ] || ln -s pts/ptmx /dev/ptmx

# Fake credentials where real ones live, each carrying the run's canary, so
# a read shows up in the trace and the content can be recognised if it goes
# anywhere. Only on a scratch home: a session's kept home is yours, and its
# .npmrc, .netrc or gh login must survive the next boot.
if [ -z "$kept_home" ]; then
CANARY_UP=$(echo "$canary" | tr a-f A-F)
mkdir -p /home/runner/.aws /home/runner/.ssh /home/runner/.config/gh /home/runner/.docker
cat >/home/runner/.aws/credentials <<EOF
[default]
aws_access_key_id = AKIASBOX$CANARY_UP
aws_secret_access_key = sbox${canary}Kf9wQ2xLr7Tz4Vb1Nm8Pc3Hd6Js5
EOF
cat >/home/runner/.ssh/id_ed25519 <<EOF
-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQsbox${canary}AAAAAAAAMwAAAAtzc2gtZW
-----END OPENSSH PRIVATE KEY-----
EOF
printf 'OPENAI_API_KEY=sk-proj-sbox%sQw7Er9Ty2Ui4\nDATABASE_URL=postgres://app:sbox%s@db:5432/app\n' "$canary" "$canary" >/home/runner/.env
printf 'machine github.com login runner password ghp_sbox%sAb1Cd2Ef3Gh4Ij5Kl6\n' "$canary" >/home/runner/.netrc
printf '//registry.npmjs.org/:_authToken=npm_sbox%sZx8Cv7Bn6Mq5\n' "$canary" >/home/runner/.npmrc
printf 'github.com:\n    user: runner\n    oauth_token: gho_sbox%sPl0Ok9Ij8Uh7\n' "$canary" >/home/runner/.config/gh/hosts.yml
printf '{"auths":{"ghcr.io":{"auth":"cnVubmVyOnNib3g%s"}}}\n' "$canary" >/home/runner/.docker/config.json
chmod 0700 /home/runner/.ssh
chmod 0600 /home/runner/.aws/credentials /home/runner/.ssh/id_ed25519 /home/runner/.env /home/runner/.netrc /home/runner/.npmrc
chown -R runner:runner /home/runner/.aws /home/runner/.ssh /home/runner/.config /home/runner/.docker \
	/home/runner/.env /home/runner/.netrc /home/runner/.npmrc
fi

# Network: the VM side of the tap is the only thing reachable.
ip link set lo up
if ip link show eth0 >/dev/null 2>&1; then
	ip addr add 172.16.0.2/30 dev eth0
	ip link set eth0 up
	ip route add default via 172.16.0.1
fi
# Lookups go to the DNS logger on the VM side of the tap. The root is
# read-only, so the new resolv.conf is bind-mounted from tmpfs.
printf 'nameserver 172.16.0.1\noptions timeout:1 attempts:1\n' >/run/sbox-resolv.conf
mount --bind /run/sbox-resolv.conf /etc/resolv.conf
# host.sbox is the VM side of the tap, where a session's services (the
# Mac's ports it may reach) are relayed.
# A hostname from the nonce: six letters, different every boot, nothing
# that says sbox.
name=$(echo "$nonce" | tr 0-9a-f g-v | cut -c1-6)
echo "$name" >/proc/sys/kernel/hostname
printf '127.0.0.1 localhost\n::1 localhost\n127.0.1.1 %s\n172.16.0.1 host.sbox\n' "$name" >/run/sbox-hosts
mount --bind /run/sbox-hosts /etc/hosts
# Unprivileged ICMP sockets, so the runner can ping (normally set by systemd).
echo "0 2147483647" >/proc/sys/net/ipv4/ping_group_range
# The project goes in once: on a kept home, the copy there is the agent's
# work and is never overwritten. Only what this boot wrote is chowned, so
# a kept home with node_modules isn't walked on every boot.
chown runner:runner /home/runner
# A live session mounts the Mac's folder itself over the work folder: sbox
# live-serve on the Mac, through the relay on 172.16.0.1:2049, which may
# still be starting. On a kept home an earlier copy stays underneath.
if [ -s /drive/sbox/live ]; then
	mkdir -p /home/runner/work
	i=0
	# The client's callback socket, unused: the server never delegates. Below
	# 1024 so the summary doesn't report it as the project's port.
	echo 1023 >/sys/module/nfs/parameters/callback_tcpport 2>/dev/null
	until mount -t nfs4 -o vers=4.0,addr=172.16.0.1,clientaddr=172.16.0.2,hard,timeo=600 172.16.0.1:/ /home/runner/work 2>/dev/null; do
		i=$((i + 1))
		if [ "$i" -ge 30 ]; then
			echo "sbox: the live mount didn't answer; ~/work is not the Mac's folder"
			break
		fi
		sleep 1
	done
	# dev.local: folders kept on the kept home's disk, bound over the
	# mount, so dependency trees stay fast and off the Mac.
	[ -f /drive/sbox/local ] && while IFS= read -r p; do
		case "$p" in "" | /* | ..* | */..*) continue ;; esac
		n=$(echo "$p" | tr / _)
		mkdir -p "/home/runner/.sbox/local/$n"
		# Only when missing: a read-only mount refuses even a folder that's there.
		[ -d "/home/runner/work/$p" ] || mkdir -p "/home/runner/work/$p"
		chown runner:runner /home/runner/.sbox /home/runner/.sbox/local "/home/runner/.sbox/local/$n"
		mount --bind "/home/runner/.sbox/local/$n" "/home/runner/work/$p"
	done </drive/sbox/local
elif [ ! -e /home/runner/work ]; then
	cp -a /drive/skill /home/runner/work
	chown -R runner:runner /home/runner/work
fi

# Prefetch phase: download dependencies onto the empty writable drive, with
# package-manager scripts disabled, through the registry-only proxy.
if [ "$phase" = prefetch ]; then
	mount /dev/vdc /mnt && chown runner:runner /mnt
	echo "SBOX-$nonce PREFETCH-BEGIN"
	cd /home/runner/work
	nonce="$nonce" sh /drive/sbox/prefetch.sh </dev/null 2>&1
	echo
	echo "SBOX-$nonce PREFETCH-END $?"
	cd /
	umount /mnt
	poweroff
	exit 0
fi

# Main phase with prefetched dependencies: the drive is read-only, so each
# folder is overlaid with a writable tmpfs layer (npm rebuild writes build
# output, npx and uv write to their caches). Wheels install offline, which
# runs no package code.
overlay() { # lower target
	[ -d "$1" ] || return 0
	n=$(echo "$2" | tr / _)
	mkdir -p "/run/ovl/$n/upper" "/run/ovl/$n/work" "$2"
	mount -t overlay overlay -o "lowerdir=$1,upperdir=/run/ovl/$n/upper,workdir=/run/ovl/$n/work" "$2"
	chown runner:runner "$2"
}
if [ -z "$kept_home" ] && [ -b /dev/vdc ]; then
	mount -o ro /dev/vdc /mnt
	[ -e /home/runner/work/node_modules ] || overlay /mnt/node_modules /home/runner/work/node_modules
	overlay /mnt/npm-cache /home/runner/.npm
	overlay /mnt/uv-cache /home/runner/.cache/uv
	chown runner:runner /home/runner/.cache 2>/dev/null
	if ls /mnt/wheels/*.whl >/dev/null 2>&1; then
		setpriv --reuid=runner --regid=runner --init-groups --reset-env \
			pip install --quiet --user --break-system-packages --no-index --no-deps /mnt/wheels/*.whl >/dev/null 2>&1
	fi
fi

# Mounts: Mac files and folders copied into the home at each boot, so the
# Mac's version wins. Parents are made by the runner, and a path that
# resolves outside the home (a link the runner left) is skipped.
if [ -f /drive/sbox/mounts ]; then
	while read -r n guest; do
		setpriv --reuid=runner --regid=runner --init-groups mkdir -p "$(dirname "$guest")"
		case $(realpath -m "$guest") in
		/home/runner/?*) ;;
		*) echo "sbox: mount $guest leaves the home, skipped"; continue ;;
		esac
		rm -rf "$guest"
		cp -a "/drive/mounts/$n" "$guest"
		chown -R runner:runner "$guest"
	done </drive/sbox/mounts
fi

# Links: other Mac folders and files, through their relays, mounted last so
# that nothing sbox writes lands on the Mac. A path that goes through a link the
# runner left on the kept home is skipped, or it would steer this mount; what's at
# a folder's path stays underneath. A file's folder shows that one name, and is
# mounted out of the way: a mount at the path would refuse a rename over it. All
# wait for their relays together.
if [ -f /drive/sbox/links ]; then
	echo 1023 >/sys/module/nfs/parameters/callback_tcpport 2>/dev/null
	tab=$(printf '\t')
	i=0
	while IFS="$tab" read -r _ port _ _ kind name guest; do
		if [ "$kind" = file ]; then
			at=/run/sbox/links/$port
			mkdir -p "$at"
			setpriv --reuid=runner --regid=runner --init-groups mkdir -p "$(dirname "$guest")" 2>/dev/null
			if [ "$(realpath -m "$(dirname "$guest")")" != "$(dirname "$guest")" ] || { [ -d "$guest" ] && [ ! -L "$guest" ]; }; then
				echo "sbox: link $guest is a folder here, or goes through a link: skipped"
				continue
			fi
		else
			at=$guest
			setpriv --reuid=runner --regid=runner --init-groups mkdir -p "$guest" 2>/dev/null
			if [ "$(realpath -m "$guest")" != "$guest" ] || [ ! -d "$guest" ]; then
				echo "sbox: link $guest isn't a folder here, or goes through a link: skipped"
				continue
			fi
		fi
		mounted=1
		until mount -t nfs4 -o "vers=4.0,addr=172.16.0.1,clientaddr=172.16.0.2,port=$port,hard,timeo=600" 172.16.0.1:/ "$at" 2>/dev/null; do
			i=$((i + 1))
			if [ "$i" -ge 30 ]; then
				echo "sbox: link $guest didn't answer; it is not the Mac's"
				mounted=
				break
			fi
			sleep 1
		done
		# Read-only: looked up each time, so a rename on the Mac simply shows. If the
		# guest replaces the link, only its own view changes.
		if [ "$kind" = file ] && [ -n "$mounted" ]; then
			setpriv --reuid=runner --regid=runner --init-groups rm -f "$guest"
			setpriv --reuid=runner --regid=runner --init-groups ln -s "$at/$name" "$guest"
		fi
	done </drive/sbox/links
fi

# Dev sessions: Claude Code's permissions and MCP servers from the config,
# merged into the runner's files by the runner, keeping Claude Code's own.
# The merge is a second of Python, so a boot skips it when nothing changed:
# no servers to keep in a file Claude Code rewrites, and the mark the last
# merge left still matches. The mark is keyed with what only root reads, so
# the runner can't forge one to keep settings it weakened. The runner's
# files are read and written as the runner: root would follow its links.
as_runner() { setpriv --reuid=runner --regid=runner --init-groups --reset-env "$@"; }
claude_mark() {
	{
		cat /drive/sbox/claude.key /drive/sbox/claude.json
		as_runner cat /home/runner/.claude/settings.json 2>/dev/null
		cat /drive/sbox/claude.key
	} | sha256sum
}
if [ -f /drive/sbox/claude.json ]; then
	if ! [ -f /drive/sbox/claude.key ] || ! grep -q '"mcpServers":{}' /drive/sbox/claude.json ||
		[ "$(claude_mark)" != "$(as_runner cat /home/runner/.sbox/claude.mark 2>/dev/null)" ]; then
		if as_runner python3 -I /drive/sbox/claude.py /drive/sbox/claude.json /home/runner 2>&1; then
			[ -f /drive/sbox/claude.key ] && claude_mark | as_runner sh -c 'mkdir -p /home/runner/.sbox && cat >/home/runner/.sbox/claude.mark'
		else
			echo "sbox: could not write Claude Code's settings"
		fi
	fi
fi

# Sessions: sshd on the tap address with the keys and host key that
# sbox-run put on the drive: the runner's, and root's, which can only run
# sync (stop flushes the work drive with it). The config is empty, so
# nothing from the image applies.
if [ -d /drive/sbox/ssh ]; then
	mkdir -p -m 0755 /run/sshd
	# With egress, logins get the proxy settings the command gets.
	proxy_login="SetEnv=SBOX=1"
	[ -s /drive/sbox/egress ] && proxy_login="SetEnv=HTTPS_PROXY=http://172.16.0.1:3128 https_proxy=http://172.16.0.1:3128 NO_PROXY=localhost,127.0.0.1,host.sbox no_proxy=localhost,127.0.0.1,host.sbox"
	# dev.env, then the secrets (so a secret wins), one NAME=value per line,
	# for every login: sshd reads the runner's ~/.ssh/environment, a link to
	# a file in tmpfs, so the values are never on the kept home.
	install -d -o runner -g runner -m 0700 /home/runner/.ssh
	rm -f /home/runner/.ssh/environment
	if [ -s /drive/sbox/env ] || [ -s /drive/sbox/secrets ]; then
		(umask 077 && cat /drive/sbox/env /drive/sbox/secrets >/run/sbox-env 2>/dev/null)
		chown runner:runner /run/sbox-env
		ln -s /run/sbox-env /home/runner/.ssh/environment
	fi
	/usr/sbin/sshd -f /dev/null -h /drive/sbox/ssh/host_ed25519 -o "$proxy_login" \
		-o ListenAddress=172.16.0.2 -o "AllowUsers=runner root" -o PermitRootLogin=forced-commands-only \
		-o PasswordAuthentication=no -o KbdInteractiveAuthentication=no \
		-o AuthorizedKeysFile=/drive/sbox/ssh/authorized_keys.%u \
		-o PermitUserEnvironment=yes -o PrintLastLog=no -o PidFile=/run/sshd.pid
fi

echo "SBOX-$nonce START"
cd /home/runner/work
if [ -f /drive/sbox/scan.sh ]; then
	# Scan mode: the script runs as root and drops each entry point to the
	# runner under strace. It prints its own markers, so it gets the nonce.
	nonce="$nonce" canary="$canary" sh /drive/sbox/scan.sh </dev/null 2>&1
else
	proxy_env=""
	[ -s /drive/sbox/egress ] && proxy_env="HTTPS_PROXY=http://172.16.0.1:3128 https_proxy=http://172.16.0.1:3128"
	# The config's secrets, NAME=value lines, as env's last assignments.
	set --
	[ -f /drive/sbox/secrets ] && while IFS= read -r kv; do set -- "$@" "$kv"; done </drive/sbox/secrets
	# shellcheck disable=SC2086 # proxy_env is two assignments or nothing
	setpriv --reuid=runner --regid=runner --init-groups --reset-env env $proxy_env "$@" sh /drive/sbox/cmd.sh </dev/null 2>&1
fi
status=$?
# sbox run -apply or -patch: the work folder's tar onto the last drive (the
# raw out drive sbox-run attached), after a block holding its byte count.
if [ -f /drive/sbox/out.sh ]; then
	out=$(ls /dev/vd? | tail -n 1)
	n=$({ sh /drive/sbox/out.sh 2>/dev/null | dd of="$out" bs=512 seek=1 iflag=fullblock conv=notrunc; } 2>&1 | sed -n 's/^\([0-9][0-9]*\) bytes.*/\1/p')
	printf '%s\n' "${n:-0}" | dd of="$out" bs=512 count=1 conv=notrunc,sync 2>/dev/null
	sync
fi
echo # the command's output may not end with a newline
echo "SBOX-$nonce EXIT $status"
poweroff
