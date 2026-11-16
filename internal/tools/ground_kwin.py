#!/usr/bin/env python3
"""Orion M2 grounding KWin helper (vendored, read-only).

Queries KWin (the Wayland compositor) for every window's TRUE
screen geometry via its scripting API, and prints one JSON
document: {"kwin_windows": [{"cls": ..., "active": ...,
"minimized": ..., "fg": [x, y, w, h], "cg": [x, y, w, h],
"sens": "<matched sensitive pattern or ''>"}]}.

Why: on Wayland a client does not know its own absolute position
(the compositor decides it), so Firefox's accessibility tree
reports window-relative coordinates — its window at (0,0) while
KWin places it at (0,29). The AT-SPI boxes are therefore ~29 px
above the visible elements (F2). KWin's frameGeometry is the
compositor's truth; the Go caller matches KWin windows to AT-SPI
windows (compatible app class + same size) and applies the
per-window translation. NEVER a hardcoded offset: the correction
is measured fresh on every grounding.

Titles NEVER leave KWin (F-M2R-15): the script's print() output
lands in the systemd journal, and window titles can carry
sensitive text (document names, account numbers, message
subjects) — so the JSON carries NO caption at all. The
sensitive-window check runs INSIDE the script instead: the
caller passes the sensitive substrings as argv, the script
matches each window's caption against them (case-insensitive
substring, same rule as the Go side) and reports only the
matched pattern ("sens"), never the title. A journal reader sees
geometries and a pattern word, never a title.

Mechanism: load a tiny JS script via org.kde.KWin /Scripting
(loadScript + start), which prints the geometries with a unique
marker; this helper polls the user journal for the marker, then
unloads the script. Reliability measured 2026-11-15: 10/10
trials, ~0.1 s each, identical geometries across trials (the M3
cursorPos oracle's 2/5 flakiness did not reproduce with unique
markers + proper load/start/unload sequencing). Note: start()
starts ALL loaded scripts (that is the KWin API — there is no
per-script start), so a foreign or broken script can fail the
start call; the defence is the unique per-run marker (pid +
random), so only lines carrying this run's marker are ever
parsed — another script's output cannot corrupt this run's
result (F-M2R-24). A script that fails to load or start is
unloaded before this helper exits, so a loaded-never-started
script never lingers to print stale geometry on the next run.

Marker (F-A23W2-11): pid + 64-bit secrets randomness +
nanosecond timestamp, echoed as "nonce" in the JSON payload and
checked on receipt — the same fix as the input cursor helper.
The old marker was 16-bit random only, and the script name was
pid-only: two calls from the same pid collided after ~100 calls
(birthday paradox), and a stale journal line would return the
wrong geometries as grounding truth.

Reads ONLY window geometries (captions are compared inside the
script for the sensitive flag and never printed). On ANY failure
this helper exits non-zero with the error on stderr and NO JSON
on stdout, so the Go caller grounds uncorrected instead of
grounding on partial or faked geometry.
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

# Sensitive substrings, passed by the Go caller as argv[1:]
# (same list as sensitiveWindowTitles in internal/tools/
# computeruse.go — the Go side is authoritative; the helper takes
# them as input so the two can never drift). The script matches
# each caption against them and reports only the matched pattern.
SENSITIVE_PATTERNS = sys.argv[1:]

JS_TEMPLATE = """var pats = %s;
var wins = workspace.windowList();
var out = [];
for (var i = 0; i < wins.length; i++) {
    var w = wins[i];
    var fg = w.frameGeometry;
    var cg = w.clientGeometry;
    var sens = "";
    var cap = (w.caption || "").toLowerCase();
    for (var k = 0; k < pats.length; k++) {
        if (cap.indexOf(pats[k]) >= 0) { sens = pats[k]; break; }
    }
    out.push({cls: w.resourceClass, active: w.active, minimized: w.minimized, fg: [fg.x, fg.y, fg.width, fg.height], cg: [cg.x, cg.y, cg.width, cg.height], sens: sens});
}
print("%s" + JSON.stringify({nonce: "%s", kwin_windows: out}));
"""

POLL_TRIES = 6
POLL_DELAY = 0.5


def fail(msg):
    print("kwin helper: " + msg, file=sys.stderr)
    sys.exit(1)


def journal_grep(marker):
    for _ in range(POLL_TRIES):
        try:
            # F-M2R-24: the journalctl timeout was 15 s — when the
            # Go caller kills this helper on its 5 s timeout, an
            # in-flight journalctl child orphaned for up to 15 s.
            # 3 s bounds the orphan; the Go side also kills the
            # helper's whole process group on timeout.
            r = subprocess.run(
                ["journalctl", "--user", "-u", "plasma-kwin_wayland.service",
                 "-n", "200", "--no-pager"],
                capture_output=True, text=True, timeout=3)
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


def main():
    marker, nonce = new_marker("ORION-KWIN-GEOM")
    js = JS_TEMPLATE % (json.dumps(SENSITIVE_PATTERNS), marker, nonce)
    fd, js_path = tempfile.mkstemp(prefix="orion-kwin-geom-", suffix=".js")
    try:
        os.write(fd, js.encode())
        os.close(fd)
        name = "orion-kwin-geom-%s" % nonce
        try:
            proxy = Gio.DBusProxy.new_for_bus_sync(
                Gio.BusType.SESSION, Gio.DBusProxyFlags.NONE, None,
                "org.kde.KWin", "/Scripting", "org.kde.kwin.Scripting", None)
        except Exception as e:
            fail("cannot reach KWin scripting: %s" % e)
        load_attempted = False
        try:
            load_attempted = True
            proxy.call_sync("loadScript",
                            GLib.Variant("(ss)", (js_path, name)),
                            Gio.DBusCallFlags.NONE, -1, None)
            proxy.call_sync("start", None,
                            Gio.DBusCallFlags.NONE, -1, None)
        except Exception as e:
            if load_attempted:
                # F-M2R-24: a loaded-never-started script used to
                # linger in KWin (unload ran only after a
                # successful start) and print stale geometry on
                # the next start. Unload it before failing — even
                # when loadScript itself threw, KWin may have
                # loaded the script first, so the unload is
                # best-effort and its own failure is ignored.
                try:
                    proxy.call_sync("unloadScript",
                                    GLib.Variant("(s)", (name,)),
                                    Gio.DBusCallFlags.NONE, -1, None)
                except Exception:
                    pass
            fail("loadScript/start failed: %s" % e)
        line = journal_grep(marker)
        try:
            proxy.call_sync("unloadScript",
                            GLib.Variant("(s)", (name,)),
                            Gio.DBusCallFlags.NONE, -1, None)
        except Exception:
            pass
        if line is None:
            fail("no KWin geometry output in the journal for %s" % marker)
        try:
            payload = line.split(marker, 1)[1].strip()
            data = json.loads(payload)
        except Exception as e:
            fail("cannot parse KWin geometry output: %s" % e)
        if not isinstance(data, dict) or "kwin_windows" not in data:
            fail("KWin geometry output has no kwin_windows")
        if data.get("nonce") != nonce:
            fail("KWin geometry output nonce mismatch: stale journal line, not this call's output")
        wins = data["kwin_windows"]
        if not isinstance(wins, list):
            fail("KWin geometry output is not a list")
        print(json.dumps({"kwin_windows": wins}))
    finally:
        try:
            os.unlink(js_path)
        except Exception:
            pass


if __name__ == "__main__":
    main()
