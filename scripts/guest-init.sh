#!/bin/sh
# Per-run guest init, started by /sbin/sbox-init from the run's drive.
# The root filesystem is read-only: writable scratch lives in tmpfs. The
# command runs as the unprivileged runner user, then the VM powers off.
# Markers carry the run's nonce, readable by root only, so the command
# can't print a fake result.
nonce=$(cat /drive/sbox/nonce)
canary=$(cat /drive/sbox/canary)
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
sync
# No systemd, so no reboot(8): ask the kernel directly. Firecracker exits on reboot.
echo 1 >/proc/sys/kernel/sysrq
echo b >/proc/sysrq-trigger
