#!/usr/bin/env python3
"""DNS logger for one sbox run.

Answers every query with NXDOMAIN and logs its type and name, one line per
query. It never forwards, so lookups fail as they would without it and
nothing leaves the run's network namespace. The exception is the names
given after the log file, a session's raw TCP egress hosts: they resolve to
the logger's own address, the VM side of the tap, where a relay carries the
connection to that host alone.

Usage: sbox-dns.py <address> <port> <log file> [name ...]
"""
import socket
import struct
import sys

TYPES = {1: "A", 2: "NS", 5: "CNAME", 6: "SOA", 12: "PTR", 15: "MX", 16: "TXT",
         28: "AAAA", 33: "SRV", 64: "SVCB", 65: "HTTPS", 255: "ANY"}
MAX_LOGGED = 10000  # a runaway loop shouldn't fill the disk
NXDOMAIN = 3


def label_text(raw):
    """Printable label: anything outside visible ASCII is escaped, so a
    crafted name can't break the one-query-per-line log."""
    return "".join(chr(b) if 33 <= b < 127 and b != 0x5C else "\\x%02x" % b for b in raw)


def parse(packet):
    """Return (id, flags, name, qtype, question bytes) or None if it isn't
    a well-formed query with one plain (uncompressed) question."""
    if len(packet) < 12:
        return None
    qid, flags, qdcount = struct.unpack("!HHH", packet[:6])
    if flags & 0x8000 or qdcount < 1:  # a response, or no question
        return None
    i, labels = 12, []
    while True:
        if i >= len(packet) or len(labels) > 127:
            return None
        n = packet[i]
        if n == 0:
            i += 1
            break
        if n & 0xC0 or i + 1 + n > len(packet):  # compression pointer, or truncated
            return None
        labels.append(label_text(packet[i + 1:i + 1 + n]))
        i += 1 + n
    if i + 4 > len(packet):
        return None
    qtype = struct.unpack("!H", packet[i:i + 2])[0]
    return qid, flags, ".".join(labels), qtype, packet[12:i + 4]


def answer(qid, flags, question):
    """NXDOMAIN for the question: QR set, opcode and RD copied, no records."""
    rflags = 0x8000 | (flags & 0x7900) | NXDOMAIN
    return struct.pack("!HHHHHH", qid, rflags, 1, 0, 0, 0) + question


def reply(qid, flags, name, qtype, question, names, address):
    """The answer for one query: for a listed name, an A record with
    address (or no data for other types); NXDOMAIN for any other name."""
    if name.lower() not in names:
        return answer(qid, flags, question)
    rflags = 0x8000 | (flags & 0x7900) | 0x0080  # QR, opcode and RD copied, RA
    if qtype != 1:
        return struct.pack("!HHHHHH", qid, rflags, 1, 0, 0, 0) + question
    record = struct.pack("!HHHIH", 0xC00C, 1, 1, 5, 4) + socket.inet_aton(address)  # name: the question's
    return struct.pack("!HHHHHH", qid, rflags, 1, 1, 0, 0) + question + record


def serve(address, port, log_path, names=frozenset()):
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.bind((address, port))
    logged = 0
    with open(log_path, "a", buffering=1) as log:
        while True:
            packet, peer = sock.recvfrom(4096)
            query = parse(packet)
            if query is None:
                continue
            qid, flags, name, qtype, question = query
            if logged < MAX_LOGGED:
                log.write("%s %s\n" % (TYPES.get(qtype, str(qtype)), name or "."))
                logged += 1
            sock.sendto(reply(qid, flags, name, qtype, question, names, address), peer)


def selftest():
    q = struct.pack("!HHHHHH", 7, 0x0100, 1, 0, 0, 0) + b"\x07example\x03com\x00" + struct.pack("!HH", 28, 1)
    qid, flags, name, qtype, question = parse(q)
    assert (qid, name, qtype) == (7, "example.com", 28), (qid, name, qtype)
    r = answer(qid, flags, question)
    assert struct.unpack("!HHHHHH", r[:12]) == (7, 0x8103, 1, 0, 0, 0)
    assert r[12:] == question
    assert parse(b"\x00" * 11) is None  # too short
    assert parse(q[:-3]) is None  # truncated question
    evil = struct.pack("!HHHHHH", 1, 0, 1, 0, 0, 0) + b"\x03a\nb\x00" + struct.pack("!HH", 1, 1)
    assert parse(evil)[2] == "a\\x0ab"  # a newline can't forge a log line

    # Names allowed raw TCP egress resolve to the VM side of the tap.
    names = {"github.com"}
    a = struct.pack("!HHHHHH", 9, 0x0100, 1, 0, 0, 0) + b"\x06GitHub\x03com\x00" + struct.pack("!HH", 1, 1)
    qid, flags, name, qtype, question = parse(a)
    r = reply(qid, flags, name, qtype, question, names, "172.16.0.1")
    assert struct.unpack("!HHHHHH", r[:12]) == (9, 0x8180, 1, 1, 0, 0), struct.unpack("!HHHHHH", r[:12])
    assert r.endswith(struct.pack("!HHIH", 1, 1, 5, 4) + socket.inet_aton("172.16.0.1"))
    aaaa = parse(a[:-4] + struct.pack("!HH", 28, 1))
    assert struct.unpack("!HHHHHH", reply(*aaaa, names, "172.16.0.1")[:12]) == (9, 0x8180, 1, 0, 0, 0)  # no data, not NXDOMAIN
    other = parse(q)
    assert reply(*other, names, "172.16.0.1") == answer(other[0], other[1], other[4])  # still NXDOMAIN
    print("ok")


if __name__ == "__main__":
    if sys.argv[1:] == ["--selftest"]:
        selftest()
    elif len(sys.argv) >= 4:
        serve(sys.argv[1], int(sys.argv[2]), sys.argv[3], {n.lower() for n in sys.argv[4:]})
    else:
        sys.exit(__doc__)
