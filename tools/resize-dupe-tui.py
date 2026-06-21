#!/usr/bin/env python3
"""
resize-dupe-tui.py — reproduce the "duplicate lines on resize" that
MAIN-SCREEN TUIs (claude-code, and anything Ink/React-for-CLI based)
cause. This is the app-re-emit case, NOT the static-reflow case (that's
tools/resize-dupe-test.txt).

What it does:
  1. Dumps one frame of N numbered lines, then GOES IDLE (no streaming —
     it just sits there, like claude-code waiting for your input).
  2. On SIGWINCH (you resize the window) it RE-EMITS the whole frame by
     moving the cursor up over what it drew and reprinting.

Why that duplicates: a terminal can move the cursor up only within the
visible screen — it CANNOT move up into scrollback. So once the frame is
taller than the window, the lines that already scrolled off the top are
NOT overwritten by the re-emit; the reprint just lays down a second copy.
Resize again and you stack another copy. That is precisely the duplicate
block you saw — claude-code re-renders its whole conversation on resize
the same way.

USAGE:
    python3 tools/resize-dupe-tui.py     # make sure the window is SHORTER
                                         # than the frame so lines scroll off
    # resize narrower/wider a few times, then Ctrl-C and scroll up:
    # any "LINE NNN" that appears more than once is the bug.

Run it in xerotty AND xfce4-terminal/kitty side by side: if they all
duplicate identically, it's the app pattern (claude-code's to fix); if
xerotty is worse, our reflow is amplifying it.
"""
import os
import sys
import signal

CSI = "\033["
LINES = 50  # taller than a normal window, so the top scrolls into scrollback


def frame(resizes):
    out = []
    out.append("================ RESIZE-DUPE TUI (idle re-emit) ================")
    out.append("resizes so far: %d   — each LINE below should appear ONCE." % resizes)
    out.append("=" * 64)
    for i in range(1, LINES + 1):
        out.append("LINE %03d  ::  the quick brown fox jumps over the lazy dog" % i)
    out.append("---- resize the window, then Ctrl-C and scroll up to inspect ----")
    return out


_drawn = 0
_resizes = 0


def draw(reprint):
    global _drawn
    f = frame(_resizes)
    if reprint and _drawn:
        # Jump up over what we last drew and reprint. The cursor clamps at
        # the top of the VISIBLE screen — it can't climb into scrollback —
        # so any line that already scrolled off is left there AND a fresh
        # copy is printed: the duplicate.
        sys.stdout.write(CSI + "%dA" % _drawn)
    for ln in f:
        sys.stdout.write("\r" + CSI + "K" + ln + "\n")
    _drawn = len(f)
    sys.stdout.flush()


def on_winch(_sig, _frame):
    global _resizes
    _resizes += 1
    draw(reprint=True)


def main():
    signal.signal(signal.SIGWINCH, on_winch)
    draw(reprint=False)  # dump the frame once, then idle
    try:
        while True:
            signal.pause()  # wake only on SIGWINCH (redraw) or SIGINT (quit)
    except KeyboardInterrupt:
        sys.stdout.write("\n[done]\n")


if __name__ == "__main__":
    main()
