#!/usr/bin/env python3
"""Keeps a read-write link to a file (docs/live-links.md) the same on both sides.

The guest's path is an ordinary file on the kept home, so every tool's way of
saving works, a rename over it included; a mount there would refuse that. This
copies it to and from the hidden mount of the Mac's file, as the runner: it can do
nothing the guest can't, and the Mac's server checks every write.

Usage: sbox-links.py [--once] <sbox/links> <home>
       sbox-links.py --selftest
"""

import errno
import hashlib
import io
import os
import stat
import sys
import tempfile
import time

TICK = 0.5  # seconds between looks at both sides
CAP = 16 << 20  # ponytail: whole-file copies and hashes; link the folder for a database or a large log
OUT = sys.stdout


class Skip(Exception):
    """This side can't be read now: not a regular file, or too large."""


def sig(path):
    """What says a file changed. None for anything that can't be looked at,
    a stale handle from a rename on the Mac included: the next look finds it."""
    try:
        st = os.lstat(path)
    except OSError:
        return None
    return (stat.S_IFMT(st.st_mode), st.st_mtime_ns, st.st_size, st.st_ino)


def read(path):
    """The file's bytes and mode bits. Never through a link, nor stuck on a pipe."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError as e:
        if e.errno == errno.ELOOP:
            raise Skip("is a symlink") from e
        raise
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            raise Skip("isn't a regular file")
        if st.st_size > CAP:
            raise Skip("is over 16 MiB")
        parts = []
        while chunk := os.read(fd, 1 << 20):
            parts.append(chunk)
        return b"".join(parts), stat.S_IMODE(st.st_mode)
    finally:
        os.close(fd)


def digest(content):
    return hashlib.sha256(content).hexdigest()


def decide(base, guest, mac):
    """What to do, from the hash each side last agreed on and each side's now.
    A side that's missing waits: a tool that unlinks before it writes mustn't take
    the other side's file with it."""
    if mac is None or (guest is None and base is not None):
        return "wait"
    if guest is None:
        return "pull"
    if guest == mac:
        return "agree"
    if base is None or (guest != base and mac != base):
        return "conflict"
    return "pull" if guest == base else "push"


class Link:
    def __init__(self, link_id, guest, mac, home):
        self.guest, self.mac, self.home = guest, mac, home
        self.base_file = os.path.join(home, ".sbox", "links", link_id)
        self.conflicts = os.path.join(home, ".sbox", "conflicts")
        self.tmp = os.path.join(os.path.dirname(guest), ".sbox-link-" + link_id + ".tmp")
        self.seen = (None, None)
        self.dirty = True
        self.said = None

    def say(self, line):
        if line != self.said:
            print("sbox: link " + self.guest + ": " + line, file=OUT, flush=True)
            self.said = line

    def base(self):
        try:
            with open(self.base_file) as f:
                return f.read().strip()
        except OSError:
            return None

    def agree(self, value):
        os.makedirs(os.path.dirname(self.base_file), exist_ok=True)
        with open(self.base_file + ".tmp", "w") as f:
            f.write(value)
        os.replace(self.base_file + ".tmp", self.base_file)

    def push(self, content):
        # In place, so the Mac's inode, mode and any link to it stay; written before
        # truncating, so there's no empty file to catch or to be left with. ponytail:
        # a reader on the Mac can catch a few ms of mixed content.
        fd = os.open(self.mac, os.O_WRONLY | os.O_NOFOLLOW)
        try:
            view = memoryview(content)
            done = 0
            while done < len(view):
                done += os.pwrite(fd, view[done:], done)
            os.ftruncate(fd, len(content))
        finally:
            os.close(fd)

    def pull(self, content, mode):
        try:
            os.unlink(self.tmp)
        except OSError:
            pass
        fd = os.open(self.tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        try:
            view = memoryview(content)
            done = 0
            while done < len(view):
                done += os.write(fd, view[done:])
            os.fchmod(fd, mode)
        finally:
            os.close(fd)
        os.replace(self.tmp, self.guest)

    def keep_guests(self, content):
        os.makedirs(self.conflicts, exist_ok=True)
        name = os.path.basename(self.guest) + "." + time.strftime("%Y%m%dT%H%M%S", time.gmtime())
        path = os.path.join(self.conflicts, name)
        with open(path, "wb") as f:
            f.write(content)
        return path

    def sync(self):
        """One comparison, now. True when both sides were found settled. What was
        seen is what came before the reads, and a copy means another look, so a change
        made during this one is never taken for seen."""
        before = (sig(self.guest), sig(self.mac))
        try:
            guest = read(self.guest) if before[0] else None
            mac = read(self.mac) if before[1] else None
            base = self.base()
            action = decide(base, digest(guest[0]) if guest else None, digest(mac[0]) if mac else None)
            # A write since those reads is the next look's, not one to overwrite.
            if action in ("pull", "conflict", "push") and (sig(self.guest), sig(self.mac)) != before:
                return False
            if action == "agree":
                self.agree(digest(mac[0]))
            elif action == "push":
                self.push(guest[0])
                self.agree(digest(guest[0]))
            elif action == "pull":
                self.pull(*mac)
                self.agree(digest(mac[0]))
            elif action == "conflict":
                self.say("changed on both sides; the Mac's stays, the guest's is in " + self.keep_guests(guest[0]))
                self.pull(*mac)
                self.agree(digest(mac[0]))
            self.seen = before
            return action in ("agree", "wait")
        except Skip as e:
            self.say(str(e) + "; not copied")
            self.seen = before
            return True
        except OSError as e:
            self.say("not copied yet: " + str(e))
            return False

    def step(self):
        """One look. A change is copied once it has held for a look, so a file being
        written isn't sent half done."""
        now = (sig(self.guest), sig(self.mac))
        if now != self.seen:
            self.seen = now
            self.dirty = True
            return
        if self.dirty:
            self.dirty = not self.sync()


def parse(lines, home):
    """The read-write file links in sbox/links: id, guest port, Mac port, ro|rw,
    dir|file, name, guest path, tab-separated."""
    links = []
    for line in lines:
        fields = line.rstrip("\n").split("\t")
        if len(fields) == 7 and fields[3] == "rw" and fields[4] == "file":
            links.append(Link(fields[0], fields[6], "/run/sbox/links/" + fields[1] + "/" + fields[5], home))
    return links


def selftest():
    global OUT
    OUT = io.StringIO()
    with tempfile.TemporaryDirectory() as d:
        n = 0

        def fresh():
            nonlocal n
            n += 1
            root = os.path.join(d, str(n))
            os.makedirs(os.path.join(root, "guest"))
            os.makedirs(os.path.join(root, "mac"))
            return Link("id", os.path.join(root, "guest", "f"), os.path.join(root, "mac", "f"), os.path.join(root, "home"))

        def write(path, content):
            with open(path, "w") as f:
                f.write(content)

        def text(path):
            with open(path) as f:
                return f.read()

        def settle(link):
            for _ in range(3):
                link.step()

        for base, guest, mac, want in [
            (None, None, None, "wait"), (None, None, "m", "pull"), (None, "g", None, "wait"), (None, "g", "m", "conflict"), (None, "s", "s", "agree"),
            ("b", "b", "b", "agree"), ("b", "g", "b", "push"), ("b", "b", "m", "pull"), ("b", "g", "m", "conflict"), ("b", "s", "s", "agree"),
            ("b", None, "b", "wait"), ("b", None, "m", "wait"), ("b", "b", None, "wait"), ("b", "g", None, "wait"),
        ]:
            assert decide(base, guest, mac) == want, (base, guest, mac, want)

        # The first look brings the Mac's in, with its mode bits.
        link = fresh()
        write(link.mac, "one\n")
        os.chmod(link.mac, 0o640)
        link.sync()
        assert text(link.guest) == "one\n" and stat.S_IMODE(os.stat(link.guest).st_mode) == 0o640

        # A change isn't copied until it has held for a look.
        settle(link)
        write(link.guest, "two, longer\n")
        link.step()
        assert text(link.mac) == "one\n", "copied before it held for a look"
        link.step()
        assert text(link.mac) == "two, longer\n"

        # Toward the Mac: in place, and shorter works.
        inode = os.stat(link.mac).st_ino
        write(link.guest, "3\n")
        settle(link)
        assert text(link.mac) == "3\n" and os.stat(link.mac).st_ino == inode

        # A rename over it, on either side.
        write(link.guest + ".t", "guest renamed\n")
        os.replace(link.guest + ".t", link.guest)
        settle(link)
        assert text(link.mac) == "guest renamed\n" and os.stat(link.mac).st_ino == inode
        write(link.mac + ".t", "mac renamed\n")
        os.replace(link.mac + ".t", link.mac)
        settle(link)
        assert text(link.guest) == "mac renamed\n" and not os.path.islink(link.guest)

        # Both changed: the Mac's stays, and the guest's is kept.
        write(link.guest, "guest's\n")
        write(link.mac, "mac's, longer\n")
        settle(link)
        assert text(link.guest) == "mac's, longer\n" and text(link.mac) == "mac's, longer\n"
        kept = os.listdir(link.conflicts)
        assert len(kept) == 1 and text(os.path.join(link.conflicts, kept[0])) == "guest's\n"

        # A deletion isn't passed on, either way.
        os.unlink(link.guest)
        settle(link)
        assert text(link.mac) == "mac's, longer\n", "a deletion in the guest reached the Mac"
        write(link.guest, "back\n")
        settle(link)
        assert text(link.mac) == "back\n"
        os.unlink(link.mac)
        settle(link)
        assert text(link.guest) == "back\n", "a deletion on the Mac reached the guest"
        write(link.mac, "back\n")
        settle(link)

        # A hard stop: the next boot's first look sends what the last one missed.
        write(link.guest, "not sent before the stop\n")
        again = Link("id", link.guest, link.mac, link.home)
        again.sync()
        assert text(link.mac) == "not sent before the stop\n"

        # A different file at the path with no record counts as both changed.
        link = fresh()
        write(link.mac, "mac\n")
        write(link.guest, "an earlier copy\n")
        link.sync()
        assert text(link.guest) == "mac\n"
        assert text(os.path.join(link.conflicts, os.listdir(link.conflicts)[0])) == "an earlier copy\n"

        # What isn't a regular file at the guest's path is never read or followed.
        link = fresh()
        write(link.mac, "mac\n")
        secret = os.path.join(os.path.dirname(link.guest), "secret")
        write(secret, "root's\n")
        os.symlink(secret, link.guest)
        assert link.sync() and link.said and text(link.mac) == "mac\n" and os.path.islink(link.guest)
        link = fresh()
        write(link.mac, "mac\n")
        os.mkfifo(link.guest)
        assert link.sync() and text(link.mac) == "mac\n", "a pipe at the path"

        # An edit on the Mac the moment after a copy is still seen.
        link = fresh()
        write(link.mac, "mac\n")
        settle(link)
        settle(link)
        agree = link.agree
        write(link.guest, "guest\n")

        def edit_after_copy(value):
            agree(value)
            if text(link.mac) == "guest\n":
                write(link.mac, "m edit\n")  # as long, so only its mtime says

        link.agree = edit_after_copy
        for _ in range(6):
            link.step()
        assert text(link.guest) == "m edit\n", "an edit right after a push was missed"

        # Over the cap on either side: left alone, and said once.
        link = fresh()
        write(link.mac, "mac\n")
        link.sync()
        with open(link.guest, "wb") as f:
            f.truncate(CAP + 1)
        assert link.sync() and link.said and text(link.mac) == "mac\n"

        # A write the Mac refuses is tried again, and doesn't count as agreed.
        link = fresh()
        write(link.mac, "mac\n")
        link.sync()
        write(link.guest, "guest\n")
        os.chmod(link.mac, 0o444)
        if os.geteuid() != 0:
            assert not link.sync() and link.said
            os.chmod(link.mac, 0o644)
            link.sync()
            assert text(link.mac) == "guest\n"

        # Only what sbox/links says is a read-write file.
        got = parse(["a\t20490\t1\tro\tfile\tf\t/home/runner/r\n", "b\t20491\t2\trw\tdir\t-\t/home/runner/d\n",
                     "c\t20492\t3\trw\tfile\tnew name\t/home/runner/w x\n", "junk\n"], "/home/runner")
        assert [(x.guest, x.mac) for x in got] == [("/home/runner/w x", "/run/sbox/links/20492/new name")], got
    print("ok", file=sys.stdout)


def main(args):
    if args == ["--selftest"]:
        selftest()
        return 0
    once = args[:1] == ["--once"]
    args = args[1:] if once else args
    if len(args) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    with open(args[0]) as f:
        links = parse(f, args[1])
    if once:
        for link in links:
            link.sync()
        return 0
    while True:
        for link in links:
            link.step()
        time.sleep(TICK)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
