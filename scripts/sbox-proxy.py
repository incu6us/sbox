#!/usr/bin/env python3
"""Registry-only egress for sbox's dependency prefetch.

serve: an HTTPS CONNECT proxy on a unix socket, in the VM's main network
       namespace. It tunnels only to the hosts given, or by default the
       package registries, on the ports given (no TLS interception), and logs
       every request as "ALLOW host:port" or "DENY host:port".
relay: inside a run's network namespace, forwards TCP connections on
       <address>:<port> to that unix socket. Unix sockets live on the
       filesystem, so the run's namespace still has no route out.
expose: the other half: listens on a unix socket and connects each client
       to <host>:<port>. A session's forwarded ports and services are a
       relay and an expose joined by a socket, one in the run's namespace
       and one outside it.

Usage: sbox-proxy.py serve <socket> <log file> [host:port ...]
       sbox-proxy.py relay <address> <port> <socket>
       sbox-proxy.py expose <socket> <host> <port>
"""
import os
import socket
import sys
import threading

ALLOWED = {
    ("registry.npmjs.org", 443),
    ("pypi.org", 443),
    ("files.pythonhosted.org", 443),
}
MAX_HEADER = 8192
MAX_LOGGED = 10000
_log_lock = threading.Lock()
_logged = 0


def pipe(src, dst):
    """Copy src to dst until either side closes, then close both."""
    try:
        while True:
            data = src.recv(65536)
            if not data:
                break
            dst.sendall(data)
    except OSError:
        pass
    finally:
        for s in (src, dst):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


def splice(a, b):
    threading.Thread(target=pipe, args=(a, b), daemon=True).start()
    pipe(b, a)
    a.close()
    b.close()


def parse_connect(head):
    """Return (host, port) from a CONNECT request line, or None."""
    try:
        line = head.split(b"\r\n", 1)[0].decode("ascii")
        method, target, _ = line.split(" ", 2)
        host, port = target.rsplit(":", 1)
        if method != "CONNECT" or not host or "/" in host:
            return None
        return host.lower().strip("[]"), int(port)
    except (ValueError, UnicodeDecodeError):
        return None


def log(log_file, verdict, target):
    global _logged
    with _log_lock:
        if _logged < MAX_LOGGED:
            safe = "".join(c if 33 <= ord(c) < 127 else "?" for c in target)
            log_file.write("%s %s\n" % (verdict, safe))
            _logged += 1


def handle(client, log_file):
    head = b""
    while b"\r\n\r\n" not in head:
        chunk = client.recv(1024)
        if not chunk or len(head) > MAX_HEADER:
            client.close()
            return
        head += chunk
    target = parse_connect(head)
    if target is None:
        log(log_file, "DENY", head.split(b"\r\n", 1)[0].decode("ascii", "replace")[:200])
        client.sendall(b"HTTP/1.1 405 Method Not Allowed\r\n\r\n")
        client.close()
        return
    name = "%s:%d" % target
    if target not in ALLOWED:
        log(log_file, "DENY", name)
        client.sendall(b"HTTP/1.1 403 Forbidden\r\n\r\n")
        client.close()
        return
    try:
        upstream = socket.create_connection(target, timeout=15)
        upstream.settimeout(None)
    except OSError:
        log(log_file, "FAIL", name)
        client.sendall(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
        client.close()
        return
    log(log_file, "ALLOW", name)
    client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
    rest = head.split(b"\r\n\r\n", 1)[1]
    if rest:
        upstream.sendall(rest)
    splice(client, upstream)


def parse_allowlist(entries):
    """Parse host:port entries; anything malformed is skipped."""
    allowed = set()
    for entry in entries:
        host, _, port = entry.lower().rpartition(":")
        if host and port.isdigit() and all(c.isalnum() or c in ".-" for c in host):
            allowed.add((host, int(port)))
    return allowed


def serve(sock_path, log_path):
    if os.path.exists(sock_path):
        os.unlink(sock_path)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(sock_path)
    server.listen(64)
    with open(log_path, "a", buffering=1) as log_file:
        while True:
            client, _ = server.accept()
            threading.Thread(target=handle, args=(client, log_file), daemon=True).start()


def relay(address, port, sock_path):
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind((address, port))
    server.listen(64)
    while True:
        client, _ = server.accept()
        try:
            upstream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            upstream.connect(sock_path)
        except OSError:
            client.close()
            continue
        threading.Thread(target=splice, args=(client, upstream), daemon=True).start()


def expose(sock_path, host, port):
    if os.path.exists(sock_path):
        os.unlink(sock_path)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(sock_path)
    server.listen(64)
    while True:
        client, _ = server.accept()
        try:
            upstream = socket.create_connection((host, port), timeout=10)
            upstream.settimeout(None)
        except OSError:
            client.close()
            continue
        threading.Thread(target=splice, args=(client, upstream), daemon=True).start()


def selftest():
    assert parse_connect(b"CONNECT registry.npmjs.org:443 HTTP/1.1\r\nHost: x\r\n\r\n") == ("registry.npmjs.org", 443)
    assert parse_connect(b"CONNECT PyPI.org:443 HTTP/1.1\r\n\r\n") == ("pypi.org", 443)
    assert parse_connect(b"GET http://evil.example/ HTTP/1.1\r\n\r\n") is None
    assert parse_connect(b"CONNECT github.com:443 HTTP/1.1\r\n\r\n") not in ALLOWED
    assert parse_connect(b"CONNECT registry.npmjs.org:80 HTTP/1.1\r\n\r\n") not in ALLOWED
    assert parse_connect(b"CONNECT junk\r\n\r\n") is None
    assert parse_allowlist(["API.anthropic.com:443", "bad host:1", "x.io:", "a.b:80"]) == {("api.anthropic.com", 443), ("a.b", 80)}
    print("ok")


if __name__ == "__main__":
    args = sys.argv[1:]
    if args == ["--selftest"]:
        selftest()
    elif len(args) >= 3 and args[0] == "serve":
        if len(args) > 3:
            ALLOWED = parse_allowlist(args[3:])
        serve(args[1], args[2])
    elif len(args) == 4 and args[0] == "relay":
        relay(args[1], int(args[2]), args[3])
    elif len(args) == 4 and args[0] == "expose":
        expose(args[1], args[2], int(args[3]))
    else:
        sys.exit(__doc__)
