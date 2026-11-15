#!/usr/bin/env python3
"""Orion M2 grounding KWin helper (vendored, read-only).

Queries KWin (the Wayland compositor) for every window's TRUE
screen geometry via its scripting API, and prints one JSON
document: {"kwin_windows": [{"caption": ..., "cls": ...,
"active": ..., "minimized": ..., "fg": [x, y, w, h],
"cg": [x, y, w, h]}]}.

Why: on Wayland a client does not know its own absolute position
(the compositor decides it), so Firefox's accessibility tree
reports window-relative coordinates — its window at (0,0) while
KWin places it at (0,29). The AT-SPI boxes are therefore ~29 px
above the visible elements (F2). KWin's frameGeometry is the
compositor's truth; the Go caller matches KWin windows to AT-SPI
windows (exact title + compatible app class + same size) and
applies the per-window translation. NEVER a hardcoded offset:
the correction is measured fresh on every grounding.

Mechanism: load a tiny JS script via org.kde.KWin /Scripting
(loadScript + start), which prints the geometries with a unique
marker; this helper polls the user journal for the marker, then
unloads the script. Reliability measured 2026-11-15: 10/10
trials, ~0.1 s each, identical geometries across trials (the M3
cursorPos oracle's 2/5 flakiness did not reproduce with unique
markers + proper load/start/unload sequencing). Note: start()
starts ALL loaded scripts (that is the KWin API); other scripts'
start is idempotent for well-behaved scripts.

Reads ONLY window geometries (captions are window titles, already
visible on screen). On ANY failure this helper exits non-zero
with the error on stderr and NO JSON on stdout, so the Go caller
grounds uncorrected instead of grounding on partial or faked
geometry.
"""
import json
import os
import random
import subprocess
import sys
import tempfile
import time

import gi
gi.require_version('Gio', '2.0')
from gi.repository import Gio, GLib

JS_TEMPLATE = """var wins = workspace.windowList();
var out = [];
for (var i = 0; i < wins.length; i++) {
    var w = wins[i];
    var fg = w.frameGeometry;
    var cg = w.clientGeometry;
    out.push({caption: w.caption, cls: w.resourceClass, active: w.active, minimized: w.minimized, fg: [fg.x, fg.y, fg.width, fg.height], cg: [cg.x, cg.y, cg.width, cg.height]});
}
print("%s" + JSON.stringify(out));
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


def main():
    marker = "ORION-KWIN-GEOM-%d-%04x:" % (os.getpid(), random.randrange(0x10000))
    js = JS_TEMPLATE % marker
    fd, js_path = tempfile.mkstemp(prefix="orion-kwin-geom-", suffix=".js")
    try:
        os.write(fd, js.encode())
        os.close(fd)
        name = "orion-kwin-geom-%d" % os.getpid()
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
            fail("no KWin geometry output in the journal for %s" % marker)
        try:
            payload = line.split(marker, 1)[1].strip()
            wins = json.loads(payload)
        except Exception as e:
            fail("cannot parse KWin geometry output: %s" % e)
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
