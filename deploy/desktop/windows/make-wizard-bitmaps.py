#!/usr/bin/env python3
"""Generate the MSI wizard bitmaps from the Orchicon app icon.

WHY THIS EXISTS. WiX's WixUI dialogs draw two bitmaps that we never supplied, so the
wizard fell back to WiX's own default art — a red/black pattern that has nothing to do
with Orchicon and, on the reporter's machine, visibly FLICKERED as the dialog repainted.
The fix is to ship our own:

    WixUIBannerBmp  493 x 58    the strip along the top of every wizard page
    WixUIDialogBmp  493 x 312   the panel on the left of the welcome/finish pages

THESE OUTPUTS ARE COMMITTED, not built. Same treatment as orchicon-desktop.ico: the
build must not depend on Python, Pillow or a font being present on the machine that
packages the MSI, and a bitmap that changes when a font is upgraded is a bitmap that
silently changes what users see. Run this by hand when the artwork changes.

    python3 deploy/desktop/windows/make-wizard-bitmaps.py

REQUIRES: Pillow, and a TrueType font for the wordmark (falls back to an icon-only
banner if none is found — the logo is the part that matters).
"""
from __future__ import annotations

import pathlib
import sys

from PIL import Image, ImageDraw, ImageFont

HERE = pathlib.Path(__file__).resolve().parent
# HERE is deploy/desktop/windows, so the repository root is THREE levels up.
# (parents[1] is deploy/ — a mistake that reads as "file not found" rather than as an
# off-by-one, which is why the source path is asserted with a clear message below.)
REPO = HERE.parents[2]
SOURCE_ICON = REPO / "cmd" / "orchicon-desktop" / "build" / "appicon.png"

BANNER = (493, 58)   # WixUI_Bmp_Banner
DIALOG = (493, 312)  # WixUI_Bmp_Dialog

# The app icon's own background, sampled from appicon.png. Using it means the icon's
# anti-aliased edges composite onto a background of exactly the colour they were drawn
# against, so no halo appears where the two meet.
BG_TOP = (15, 31, 26)
BG_BOTTOM = (10, 21, 18)
WORDMARK = (236, 245, 242)

# WiX wants a real BMP. 24-bit RGB (no alpha channel): an alpha BMP is accepted by some
# tools and silently rendered with a black background by others, and the wizard
# background is not always white.
def save_bmp(im: Image.Image, path: pathlib.Path) -> None:
    im.convert("RGB").save(path, "BMP")
    print(f"  wrote {path.relative_to(REPO)}  {im.size[0]}x{im.size[1]}  {path.stat().st_size} bytes")


def gradient(size: tuple[int, int]) -> Image.Image:
    """A vertical gradient, painted one row at a time — small enough to be free."""
    w, h = size
    im = Image.new("RGB", size, BG_TOP)
    d = ImageDraw.Draw(im)
    for y in range(h):
        t = y / max(h - 1, 1)
        d.line(
            [(0, y), (w, y)],
            fill=(
                round(BG_TOP[0] + (BG_BOTTOM[0] - BG_TOP[0]) * t),
                round(BG_TOP[1] + (BG_BOTTOM[1] - BG_TOP[1]) * t),
                round(BG_TOP[2] + (BG_BOTTOM[2] - BG_TOP[2]) * t),
            ),
        )
    return im


def load_font(px: int) -> ImageFont.FreeTypeFont | None:
    for candidate in (
        "/usr/share/fonts/liberation/LiberationSans-Bold.ttf",
        "/usr/share/fonts/TTF/DejaVuSans-Bold.ttf",
        "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
    ):
        if pathlib.Path(candidate).exists():
            return ImageFont.truetype(candidate, px)
    return None


def scaled_icon(height: int) -> Image.Image:
    icon = Image.open(SOURCE_ICON).convert("RGBA")
    w = round(icon.width * height / icon.height)
    return icon.resize((w, height), Image.LANCZOS)


def make_banner() -> Image.Image:
    w, h = BANNER
    im = gradient(BANNER)
    logo = scaled_icon(h - 12)  # 6px clear above and below
    im.paste(logo, (10, 6), logo)

    font = load_font(22)
    if font is not None:
        d = ImageDraw.Draw(im)
        text = "Orchicon"
        # +4 because "Orchicon" has no descender; keeps optical centring rather than
        # metric centring, which sits a couple of pixels high.
        d.text((10 + logo.width + 12, (h - 22) // 2 - 2 + 4), text, font=font, fill=WORDMARK)
    return im


def make_dialog() -> Image.Image:
    w, h = DIALOG
    im = gradient(DIALOG)
    logo = scaled_icon(int(h * 0.56))
    im.paste(logo, ((w - logo.width) // 2, (h - logo.height) // 2), logo)

    font = load_font(26)
    if font is not None:
        d = ImageDraw.Draw(im)
        text = "Orchicon"
        tw = d.textlength(text, font=font)
        d.text(
            ((w - tw) / 2, (h - logo.height) / 2 + logo.height + 14),
            text,
            font=font,
            fill=WORDMARK,
        )
    return im


def main() -> int:
    if not SOURCE_ICON.exists():
        print(f"missing source icon: {SOURCE_ICON}", file=sys.stderr)
        return 2
    print("generating WiX wizard bitmaps:")
    save_bmp(make_banner(), HERE / "wizard-banner.bmp")
    save_bmp(make_dialog(), HERE / "wizard-dialog.bmp")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
