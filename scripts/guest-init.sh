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
for d in /tmp /var/tmp /run /home/runner; do
	mount -t tmpfs -o mode=1777,size=256m tmpfs "$d"
done
chmod 0755 /home/runner

# Fake credentials where real ones live, each carrying the run's canary, so
# a read shows up in the trace and the content can be recognised if it goes
# anywhere. They exist only in this throwaway VM.
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
# Unprivileged ICMP sockets, so the runner can ping (normally set by systemd).
echo "0 2147483647" >/proc/sys/net/ipv4/ping_group_range
cp -a /drive/skill /home/runner/work
chown -R runner:runner /home/runner

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
if [ -b /dev/vdc ]; then
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

echo "SBOX-$nonce START"
cd /home/runner/work
if [ -f /drive/sbox/scan.sh ]; then
	# Scan mode: the script runs as root and drops each entry point to the
	# runner under strace. It prints its own markers, so it gets the nonce.
	nonce="$nonce" canary="$canary" sh /drive/sbox/scan.sh </dev/null 2>&1
else
	setpriv --reuid=runner --regid=runner --init-groups --reset-env sh /drive/sbox/cmd.sh </dev/null 2>&1
fi
status=$?
echo # the command's output may not end with a newline
echo "SBOX-$nonce EXIT $status"
poweroff
