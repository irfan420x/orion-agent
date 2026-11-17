#!/usr/bin/env python3
"""Orion M2 grounding helper (vendored, read-only).

Walks the AT-SPI desktop tree and prints one JSON document:
{"windows": [...], "elements": [...], "focused_window": {...},
 "walked_nodes": N, "truncated": bool}.

Reads ONLY name / role / screen extents / state (SHOWING, FOCUSED).
It NEVER reads text content or values — there is no get_text_iface
anywhere in this file — so a password field's value is never touched:
geometry + accessible name + role + state only. Each element also
carries "win", the index of its window in the "windows" array, so the
caller can apply per-window corrections (F2).

Bounds (argv): depth cap, node cap. The Go caller passes both so a
heavy app (big page, IDE) cannot hang the walk. On ANY failure the
helper exits non-zero with the error on stderr and NO JSON on
stdout, so the Go caller degrades cleanly to vision_only instead
of grounding on partial data.

Walk budget (F-A23-W4-1 / F-M2R-12, Audit 23): the old walk spent
one GLOBAL node budget in desktop order, so plasmashell+KRunner
(~839 nodes on the live desktop) ate the 1,000-node cap before
the second Firefox window was ever walked — its content was
structurally invisible to ground. Now the windows are enumerated
FIRST, then walked in priority order (focused window first) with
a PER-WINDOW node budget: the focused window gets a double share
(it is usually the target), every other window one share, and the
shares always sum to exactly MAX_NODES, so the total bound is
unchanged. A window that hits its share stops early and reports
"truncated": true (per window and top-level), so the caller can
disclose the truncation instead of silently listing a partial
desktop. Depth pruning (depth > MAX_DEPTH) is NOT truncation —
it is the design's depth bound, and is not reported as such.

Model: ~/workspace/orion-verification/computer-use-m2/design/
probe-firefox.py (bounded walk, name/role/extents only), generalised
from "Firefox only" to "every app's every window".
"""
import json
import sys
import time

import gi
gi.require_version('Atspi', '2.0')
from gi.repository import Atspi

MAX_DEPTH = int(sys.argv[1]) if len(sys.argv) > 1 else 12
MAX_NODES = int(sys.argv[2]) if len(sys.argv) > 2 else 1000


def try_twice(fn):
    """Run fn, retrying once after a short pause (F-M2R-23): a
    single flaky D-Bus call must not lose the whole tree. The
    second failure propagates — the caller decides whether to
    skip that subtree (desktop/app) or fail the helper (no
    desktop count at all means there is no tree to salvage)."""
    try:
        return fn()
    except Exception:
        time.sleep(0.05)
        return fn()


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
    showing_unknown = [0]
    elements = []

    def walk(acc, depth, win_idx, budget, count):
        """Walk one window's subtree. count is a one-element list
        holding the nodes walked in THIS window. Returns True when
        the walk stopped early because this window's node budget
        ran out (truncation); depth pruning returns False."""
        if count[0] >= budget:
            return True
        if depth > MAX_DEPTH:
            return False
        count[0] += 1
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
            # FOCUSED comes from the SAME state-set query (one
            # D-Bus call, no extra cost) and is what the
            # post-click element re-check reads (F-A23W2-9
            # follow-up): which control took focus from the press.
            # A missing key means the query failed, same as
            # SHOWING — the element is never treated as focused.
            # The window frame itself also reports FOCUSED when
            # its window is active; the Go side skips role
            # "frame" when reading the focused element, since
            # that flag duplicates the KWin window signal.
            showing = None
            focused = None
            try:
                ss = acc.get_state_set()
            except Exception:
                ss = None
            if ss is not None:
                try:
                    showing = ss.contains(Atspi.StateType.SHOWING)
                except Exception:
                    showing = None
                try:
                    focused = ss.contains(Atspi.StateType.FOCUSED)
                except Exception:
                    focused = None
            if showing is None:
                # F-M2R-21: the state query failed, so the Go side
                # keeps this element on a missing signal — count
                # it, so the summary says how many elements were
                # kept unverified instead of silently disabling
                # the filter for them.
                showing_unknown[0] += 1
            el = {"role": role, "name": name[:60],
                  "x": box[0], "y": box[1],
                  "w": box[2], "h": box[3], "win": win_idx}
            if showing is not None:
                el["showing"] = showing
            if focused is not None:
                el["focused"] = focused
            # Name only (the accessible label, e.g. "Password");
            # never a value — the helper has no way to read one.
            elements.append(el)
        try:
            n = acc.get_child_count()
        except Exception:
            return False
        hit = False
        for i in range(n):
            try:
                child = acc.get_child_at_index(i)
            except Exception:
                continue
            if child is not None and walk(child, depth + 1, win_idx,
                                         budget, count):
                hit = True
                if count[0] >= budget:
                    break
        return hit

    # Enumerate the windows FIRST, without walking any subtree:
    # the per-window budget split needs the window count, and the
    # walk order (focused first) needs to know which window is
    # focused before any node is spent. win_idx is the index into
    # the "windows" array below: every element carries the window
    # it was walked from, so the Go side can apply a per-window
    # coordinate correction (F2: a Wayland client reports
    # window-relative coords, and only KWin knows the window's
    # true screen position — the offset differs per window, so
    # elements must be tagged, never blanket-shifted). The
    # windows array itself stays in desktop order; only the WALK
    # order is prioritised.
    # F-M2R-23: the outer enumeration used to call get_desktop /
    # get_child_count unwrapped — one flaky D-Bus call raised
    # straight out of main, losing the whole tree. Each outer
    # call is now retried once (try_twice) and, failing that,
    # the affected desktop is skipped (not the whole walk).
    # Only a failing desktop count is fatal (no tree to salvage).
    windows = []
    win_accs = []
    try:
        n_desks = try_twice(Atspi.get_desktop_count)
    except Exception as e:
        print("atspi helper: get_desktop_count failed: %s" % e, file=sys.stderr)
        sys.exit(1)
    for d in range(n_desks):
        try:
            desk = try_twice(lambda: Atspi.get_desktop(d))
        except Exception:
            continue
        try:
            n_apps = try_twice(desk.get_child_count)
        except Exception:
            continue
        for i in range(n_apps):
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
                windows.append({
                    "title": title[:200], "app": app_name, "role": wrole,
                    "focused": focused,
                    "x": box[0] if box else -1, "y": box[1] if box else -1,
                    "w": box[2] if box else -1, "h": box[3] if box else -1,
                    "nodes": 0, "truncated": False,
                })
                win_accs.append(win)

    # Per-window budgets: one share per window, a double share for
    # the focused window (usually the target), shares summing to
    # exactly MAX_NODES so the total bound is unchanged. With no
    # windows there is nothing to walk.
    nwin = len(windows)
    if nwin > 0:
        # units: one per window plus one extra for the focused
        # window's double share.
        units = nwin + 1
        base = MAX_NODES // units
        if base < 1:
            base = 1
        focused_idx = None
        for idx, w in enumerate(windows):
            if w["focused"]:
                focused_idx = idx
                break
        budgets = [base] * nwin
        if focused_idx is not None:
            # Focused window gets the remainder, so the shares
            # sum to exactly MAX_NODES (floored at 1: with more
            # windows than nodes the shares cannot all be >= 1).
            budgets[focused_idx] = max(1, MAX_NODES - base * (nwin - 1))
        else:
            budgets[0] += MAX_NODES - base * nwin
        # Walk order: focused window first, then the rest in
        # desktop order — so the target window's elements head
        # the list even when the element cap bites.
        order = sorted(range(nwin),
                       key=lambda i: (i != focused_idx, i))
        truncated = False
        walked = 0
        for idx in order:
            count = [0]
            hit = walk(win_accs[idx], 0, idx, budgets[idx], count)
            windows[idx]["nodes"] = count[0]
            windows[idx]["truncated"] = hit
            walked += count[0]
            if hit:
                truncated = True
    else:
        walked = 0
        truncated = False

    focused = None
    for w in windows:
        if w["focused"]:
            focused = {"title": w["title"], "app": w["app"]}
            break
    print(json.dumps({"windows": windows, "elements": elements,
                      "focused_window": focused,
                      "walked_nodes": walked,
                      "truncated": truncated,
                      "showing_unknown": showing_unknown[0]}))


if __name__ == "__main__":
    main()
