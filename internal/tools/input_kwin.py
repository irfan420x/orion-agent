#!/usr/bin/env python3
"""Orion M3 input KWin helper (vendored, read-only).

Queries KWin (the Wayland compositor) for the cursor's TRUE screen
position plus the virtual screen geometry and every window's true
geometry, and prints one JSON document:
{"cursor": {"x": ..., "y": ...},
 "screen": {"x": ..., "y": ..., "w": ..., "h": ...} | null,
 "kwin_windows": [{"caption": ..., "cls": ..., "active": ...,
                   "minimized": ..., "fg": [x, y, w, h],
                   "cg": [x, y, w, h]}]}.

Why KWin, not xdotool: the XWayland pointer is DIVERGED from the
Wayland cursor on this system (live M3 probe: xdotool stuck at
(960,672) while KWin reported (134,36)) — xdotool is NOT a valid
oracle for the Wayland cursor. KWin's workspace.cursorPos is the
compositor's truth and is the position source for the input
tool's closed-loop moves (read -> move -> re-read -> correct).

Screen geometry comes from workspace.virtualScreenGeometry — the
compositor's own screen size — so coordinate validation is
checked against a real source, never a hardcoded 1920x1080. If
that property is absent (older KWin), "screen" is null and the
Go caller validates only non-negativity; the closed loop still
fails honestly when a target is unreachable. The window list is
the same shape ground_kwin.py returns: the input tool matches
it against the AT-SPI windows to correct their boxes (the M2 F2
correction) before deciding whether a click target lies inside
a sensitive window.

Mechanism: the ground_kwin.py invocation pattern, unchanged —
load a tiny JS script via org.kde.KWin /Scripting (loadScript +
start), which prints the payload with a unique marker; this
helper polls the user journal for the marker, then unloads the
script. That pattern measured 10/10 reliable at ~0.1 s per call
for ground; the M3 one-shot cursorPos probe's 2/5 flakiness did
not reproduce with unique markers + proper load/start/unload
sequencing. Note: start() starts ALL loaded scripts (that is
the KWin API); other scripts' start is idempotent for
well-behaved scripts.

Marker (F-A23W2-11): pid + 64-bit secrets randomness +
nanosecond timestamp, echoed as "nonce" in the JSON payload and
checked on receipt. The old marker was 16-bit random only: two
calls from the same pid collided after ~100 calls (birthday
paradox), and a stale journal line would return the wrong cursor
as closed-loop truth. A stale line can no longer match: the
marker is unique per call, and even a line that contained it
would fail the payload-nonce check.

Reads ONLY cursor position, screen geometry and window
geometries (captions are window titles, already visible on
screen). On ANY failure this helper exits non-zero with the
error on stderr and NO JSON on stdout, so the Go caller fails
the move instead of moving on partial or faked geometry.
"""
import json
import os
import secrets
import subprocess
import sys
import tempfile
import time

import gi
gi.require_version('Gio', '2.0')
from gi.repository import Gio, GLib

JS_TEMPLATE = """var cp = workspace.cursorPos;
var g = null;
try {
    var vg = workspace.virtualScreenGeometry;
    if (vg) { g = {x: vg.x, y: vg.y, w: vg.width, h: vg.height}; }
} catch (e) {}
var wins = workspace.windowList();
var out = [];
for (var i = 0; i < wins.length; i++) {
    var w = wins[i];
    var fg = w.frameGeometry;
    var cg = w.clientGeometry;
    out.push({caption: w.caption, cls: w.resourceClass, active: w.active, minimized: w.minimized, fg: [fg.x, fg.y, fg.width, fg.height], cg: [cg.x, cg.y, cg.width, cg.height]});
}
print("%s" + JSON.stringify({nonce: "%s", cursor: {x: cp.x, y: cp.y}, screen: g, kwin_windows: out}));
"""

POLL_TRIES = 6
POLL_DELAY = 0.5


def fail(msg):
    print("kwin helper: " + msg, file=sys.stderr)
    sys.exit(1)


def journal_grep(marker):
    for _ in range(POLL_TRIES):
        try:
            r = subprocess.run(
                ["journalctl", "--user", "-u", "plasma-kwin_wayland.service",
                 "-n", "200", "--no-pager"],
                capture_output=True, text=True, timeout=15)
        except Exception:
            return None
        for line in r.stdout.splitlines():
            if marker in line:
                return line
        time.sleep(POLL_DELAY)
    return None


def new_marker(prefix):
    """A fresh oracle marker and its nonce: pid + 64-bit secrets
    randomness + nanosecond timestamp. The nonce is echoed in
    the payload so a stale journal line can never pass as this
    call's output (F-A23W2-11)."""
    nonce = "%d-%s-%d" % (os.getpid(), secrets.token_hex(8), time.time_ns())
    return "%s-%s:" % (prefix, nonce), nonce


def kwin_script_dir():
    """Directory for the KWin script: ~/.orion, NOT /tmp.

    oriond runs with PrivateTmp=true, so KWin — outside the
    service's private /tmp namespace — cannot read a script
    handed to loadScript from /tmp: loadScript silently loads
    nothing and the oracle never runs. ~/.orion is outside any
    private tmp (the Go installer puts the helpers themselves
    there), so KWin can read it.
    """
    home = os.path.expanduser("~")
    if not home or home == "~":
        raise RuntimeError("cannot determine the home directory")
    d = os.path.join(home, ".orion")
    os.makedirs(d, mode=0o700, exist_ok=True)
    return d


def main():
    marker, nonce = new_marker("ORION-INPUT-CURSOR")
    js = JS_TEMPLATE % (marker, nonce)
    try:
        script_dir = kwin_script_dir()
    except Exception as e:
        fail("cannot prepare the KWin script directory: %s" % e)
    # ~/.orion, not /tmp: see kwin_script_dir.
    fd, js_path = tempfile.mkstemp(prefix="orion-input-cursor-", suffix=".js", dir=script_dir)
    try:
        os.write(fd, js.encode())
        os.close(fd)
        # A per-call random suffix: a previous call whose unload
        # failed would leave a stale script behind, and loadScript
        # with a duplicate name would return the stale one instead
        # of loading this call's script.
        name = "orion-input-cursor-%s" % nonce
        try:
            proxy = Gio.DBusProxy.new_for_bus_sync(
                Gio.BusType.SESSION, Gio.DBusProxyFlags.NONE, None,
                "org.kde.KWin", "/Scripting", "org.kde.kwin.Scripting", None)
        except Exception as e:
            fail("cannot reach KWin scripting: %s" % e)
        try:
            proxy.call_sync("loadScript",
                            GLib.Variant("(ss)", (js_path, name)),
                            Gio.DBusCallFlags.NONE, -1, None)
            proxy.call_sync("start", None,
                            Gio.DBusCallFlags.NONE, -1, None)
        except Exception as e:
            fail("loadScript/start failed: %s" % e)
        line = journal_grep(marker)
        try:
            proxy.call_sync("unloadScript",
                            GLib.Variant("(s)", (name,)),
                            Gio.DBusCallFlags.NONE, -1, None)
        except Exception:
            pass
        if line is None:
            fail("no KWin cursor output in the journal for %s" % marker)
        try:
            payload = line.split(marker, 1)[1].strip()
            data = json.loads(payload)
        except Exception as e:
            fail("cannot parse KWin cursor output: %s" % e)
        if not isinstance(data, dict) or "cursor" not in data:
            fail("KWin cursor output has no cursor")
        if data.get("nonce") != nonce:
            fail("KWin cursor output nonce mismatch: stale journal line, not this call's output")
        print(json.dumps(data))
    finally:
        try:
            os.unlink(js_path)
        except Exception:
            pass


if __name__ == "__main__":
    main()
