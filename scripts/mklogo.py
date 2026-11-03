#!/usr/bin/env python3
"""Generate oline's logo assets.

Two different source images for two different jobs:

  assets/oline.ico  <- o.jpg
      The Windows .exe icon: a square tile (red mark on black, white around the
      rounded corners). The white is made transparent so the icon is a rounded
      tile. Multi-size, with alpha. The build scripts feed this to `rsrc`.

  assets/logo.ans   <- docs/logo.png
      The bigger banner logo shown on the TUI start screen, rendered as a
      truecolor half-block (U+2580) ANSI image; each cell packs two vertical
      pixels for extra detail. It is rendered FAITHFULLY on its own pure-black
      background. The TUI forces its whole background to pure black too, so the
      logo's black blends seamlessly into the app with no mismatched box.

Requires Pillow. Run from the repo root (e.g. inside WSL):
    python3 scripts/mklogo.py
"""
import os
from PIL import Image

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ICON_SRC = os.path.join(ROOT, "o.jpg")
BANNER_SRC = os.path.join(ROOT, "docs", "logo.png")
ICO = os.path.join(ROOT, "assets", "oline.ico")
ANS = os.path.join(ROOT, "assets", "logo.ans")
RASTER = os.path.join(ROOT, "assets", "logo.png")  # true PNG for the Sixel splash

ICO_SIZES = [(16, 16), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)]
ANS_COLS = 76       # banner width in terminal cells (higher = less pixelated)
ANS_BG_LUM = 20     # pixels dimmer than this are snapped to the background color
BG_RGB = (12, 12, 12)  # Windows Terminal's default dark background (#0c0c0c)


def icon_alpha(r, g, b):
    """White corners -> transparent; colored/black -> opaque, soft edge."""
    m = min(r, g, b)
    if m >= 180:
        return 0
    if m <= 120:
        return 255
    return int(round(255 * (180 - m) / 60.0))


def write_ico():
    src = Image.open(ICON_SRC).convert("RGB")
    w, h = src.size
    px = src.load()
    out = Image.new("RGBA", (w, h))
    op = out.load()
    for y in range(h):
        for x in range(w):
            r, g, b = px[x, y]
            op[x, y] = (r, g, b, icon_alpha(r, g, b))
    out.save(ICO, sizes=ICO_SIZES)
    print("wrote", os.path.relpath(ICO, ROOT), [s[0] for s in ICO_SIZES])


def _snap(p):
    """Snap near-black pixels to the background color so the logo's backdrop
    exactly matches the app/terminal background (no mismatched box)."""
    if 0.299 * p[0] + 0.587 * p[1] + 0.114 * p[2] < ANS_BG_LUM:
        return BG_RGB
    return p


def write_ans():
    src = Image.open(BANNER_SRC).convert("RGB")
    w, h = src.size
    # Half-block cells pack 2 vertical pixels each, so a cell is ~square; size
    # the pixel grid to the image's aspect at the chosen column count.
    rows = max(1, round(ANS_COLS * h / w / 2))
    img = src.resize((ANS_COLS, rows * 2), Image.LANCZOS)
    p = img.load()
    lines = []
    for r in range(rows):
        cells = []
        for x in range(ANS_COLS):
            tr, tg, tb = _snap(p[x, 2 * r])       # top pixel -> foreground
            br, bg, bb = _snap(p[x, 2 * r + 1])   # bottom pixel -> background
            cells.append(f"\x1b[38;2;{tr};{tg};{tb};48;2;{br};{bg};{bb}m▀")
        cells.append("\x1b[0m")
        lines.append("".join(cells))
    with open(ANS, "w", encoding="utf-8") as f:
        f.write("\n".join(lines))
    print("wrote", os.path.relpath(ANS, ROOT), f"{ANS_COLS}x{rows} cells")


def write_raster():
    # docs/logo.png is actually WebP; re-encode a true PNG for Go's image/png.
    Image.open(BANNER_SRC).convert("RGB").save(RASTER, "PNG")
    print("wrote", os.path.relpath(RASTER, ROOT))


if __name__ == "__main__":
    write_ico()
    write_ans()
    write_raster()
