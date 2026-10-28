#!/usr/bin/env python3
"""Redacts a session's secrets from text on its way to the Mac.

Usage: sbox-redact.py <secrets file> < text > text   (in the VM, as root)

The secrets file is the session's NAME=value lines. Each value, its hex
encoding (either case) and its base64 encodings (standard and URL-safe,
with or without padding) become [redacted:NAME]. Values shorter than 8
characters are left alone: they would match noise.
"""
import base64
import re
import sys


def patterns(secrets):
    """One regular expression per secret, longest forms first."""
    pats = []
    for name, value in secrets.items():
        if len(value) < 8:
            continue
        raw = value.encode()
        forms = {value}
        for enc in (base64.b64encode(raw), base64.urlsafe_b64encode(raw)):
            text = enc.decode()
            forms.update({text, text.rstrip("=")})
        alts = "|".join(re.escape(f) for f in sorted(forms, key=len, reverse=True))
        # hex is matched in either case; the other forms exactly.
        pats.append((re.compile(alts), re.compile(re.escape(raw.hex()), re.IGNORECASE), "[redacted:%s]" % name))
    return pats


def redact(text, pats):
    for exact, hexed, label in pats:
        text = hexed.sub(label, exact.sub(label, text))
    return text


def load(path):
    secrets = {}
    with open(path) as f:
        for line in f:
            name, sep, value = line.rstrip("\n").partition("=")
            if sep:
                secrets[name] = value
    return secrets


def selftest():
    secret = "ghp_abcdefgh12345678"
    pats = patterns({"GITHUB_TOKEN": secret, "SHORT": "abc"})
    for encoded in [secret, secret.encode().hex(), secret.encode().hex().upper(),
                    "Z2hwX2FiY2RlZmdoMTIzNDU2Nzg=", "Z2hwX2FiY2RlZmdoMTIzNDU2Nzg"]:
        out = redact("A " + encoded + ".evil.example\n", pats)
        assert out == "A [redacted:GITHUB_TOKEN].evil.example\n", (encoded, out)
    assert redact("abc stays\n", pats) == "abc stays\n"
    print("ok")


if __name__ == "__main__":
    if sys.argv[1:] == ["--selftest"]:
        selftest()
    elif len(sys.argv) == 2:
        pats = patterns(load(sys.argv[1]))
        for line in sys.stdin.buffer:
            sys.stdout.write(redact(line.decode("utf-8", "replace"), pats))
    else:
        sys.exit("usage: sbox-redact.py <secrets file> | --selftest")
