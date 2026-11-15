#!/usr/bin/env python3
"""Orion M2 grounding helper (vendored, read-only).

Walks the AT-SPI desktop tree and prints one JSON document:
{"windows": [...], "elements": [...], "focused_window": {...},
 "walked_nodes": N}.

Reads ONLY name / role / screen extents. It NEVER reads text
content or values — there is no get_text_iface anywhere in this
file — so a password field's value is never touched: geometry +
accessible name + role only. Each element also carries "win", the
index of its window in the "windows" array, so the caller can
apply per-window corrections (F2).

Bounds (argv): depth cap, node cap. The Go caller passes both so a
heavy app (big page, IDE) cannot hang the walk. On ANY failure the
helper exits non-zero with the error on stderr and NO JSON on
stdout, so the Go caller degrades cleanly to vision_only instead
of grounding on partial data.

Model: ~/workspace/orion-verification/computer-use-m2/design/
probe-firefox.py (bounded walk, name/role/extents only), generalised
from "Firefox only" to "every app's every window".
"""
import json
import sys

import gi
gi.require_version('Atspi', '2.0')
from gi.repository import Atspi

MAX_DEPTH = int(sys.argv[1]) if len(sys.argv) > 1 else 12
MAX_NODES = int(sys.argv[2]) if len(sys.argv) > 2 else 1000


def extents(acc):
    """Screen extents (x, y, w, h) or None when the node has none."""
    try:
        comp = acc.get_component_iface()
        if comp is None:
            return None
        ext = comp.get_extents(Atspi.CoordType.SCREEN)
        return (ext.x, ext.y, ext.width, ext.height)
    except Exception:
        return None


def main():
    Atspi.init()
    nodes = [0]
    windows = []
    elements = []

    def walk(acc, depth, win_idx):
        if nodes[0] >= MAX_NODES or depth > MAX_DEPTH:
            return
        nodes[0] += 1
        try:
            name = acc.get_name() or ""
        except Exception:
            name = ""
        try:
            role = acc.get_role_name() or ""
        except Exception:
            role = ""
        box = extents(acc)
        if box is not None:
            # SHOWING distinguishes elements that are actually on
            # screen from hidden-popup elements that stay in the
            # AT-SPI tree after the popup closes (calendar days,
            # session buttons). Without this the 60-element default
            # cap is exhausted by hidden plasmashell elements and
            # the focused app's elements never reach the provider
            # (live 2026-11-15 20:26: 60/60 listed were hidden
            # plasmashell, Firefox's "Sign in" absent). The Go side
            # skips showing=false; a missing key (state query
            # failed) is kept, never dropped.
            showing = None
            try:
                ss = acc.get_state_set()
                if ss is not None:
                    showing = ss.contains(Atspi.StateType.SHOWING)
            except Exception:
                showing = None
            el = {"role": role, "name": name[:120],
                  "x": box[0], "y": box[1],
                  "w": box[2], "h": box[3], "win": win_idx}
            if showing is not None:
                el["showing"] = showing
            # Name only (the accessible label, e.g. "Password");
            # never a value — the helper has no way to read one.
            elements.append(el)
        try:
            n = acc.get_child_count()
        except Exception:
            return
        for i in range(n):
            try:
                child = acc.get_child_at_index(i)
            except Exception:
                continue
            if child is not None:
                walk(child, depth + 1, win_idx)

    # win_idx is the index into the "windows" array below: every
    # element carries the window it was walked from, so the Go side
    # can apply a per-window coordinate correction (F2: a Wayland
    # client reports window-relative coords, and only KWin knows the
    # window's true screen position — the offset differs per
    # window, so elements must be tagged, never blanket-shifted).
    win_idx = -1
    for d in range(Atspi.get_desktop_count()):
        desk = Atspi.get_desktop(d)
        for i in range(desk.get_child_count()):
            try:
                app = desk.get_child_at_index(i)
            except Exception:
                continue
            if app is None:
                continue
            try:
                app_name = app.get_name() or ""
            except Exception:
                app_name = ""
            try:
                nw = app.get_child_count()
            except Exception:
                nw = 0
            for j in range(nw):
                try:
                    win = app.get_child_at_index(j)
                except Exception:
                    continue
                if win is None:
                    continue
                try:
                    title = win.get_name() or ""
                except Exception:
                    title = ""
                try:
                    wrole = win.get_role_name() or ""
                except Exception:
                    wrole = ""
                focused = False
                try:
                    ss = win.get_state_set()
                    focused = bool(ss) and ss.contains(Atspi.StateType.FOCUSED)
                except Exception:
                    pass
                box = extents(win)
                win_idx += 1
                windows.append({
                    "title": title[:200], "app": app_name, "role": wrole,
                    "focused": focused,
                    "x": box[0] if box else -1, "y": box[1] if box else -1,
                    "w": box[2] if box else -1, "h": box[3] if box else -1,
                })
                walk(win, 0, win_idx)

    focused = None
    for w in windows:
        if w["focused"]:
            focused = {"title": w["title"], "app": w["app"]}
            break
    print(json.dumps({"windows": windows, "elements": elements,
                      "focused_window": focused,
                      "walked_nodes": nodes[0]}))


if __name__ == "__main__":
    main()
