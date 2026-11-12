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

# THE SIZES ARE THE DIALOG LAYOUT'S, AND GETTING THEM WRONG IS SILENT.
#
# These were 493x58 / 493x312 — the BURN standard-BA theme's sizes, which is a different
# wizard. WixUI (the MSI's wizard) draws its banner into a 370x44 control and its dialog panel
# into 370x234:
#
#   LicenseAgreementDlg: <Control Id="BannerBitmap" Width="370" Height="44" />
#   WelcomeDlg:         <Control Id="Bitmap"       Width="370" Height="234" />
#
# So a 493-wide bitmap was being SCALED into a 370-wide box — squashed, which is what the
# operator saw ("misplaced alignment on text"), with the left edge smeared into an odd strip.
BANNER = (370, 44)   # WixUI_Bmp_Banner
DIALOG = (370, 234)  # WixUI_Bmp_Dialog

# THE DIALOG DRAWS ITS OWN TEXT ON TOP OF THESE BITMAPS, IN THE SYSTEM'S TEXT COLOUR — which is
# near-black. Artwork behind that text must therefore be LIGHT, or the wizard is unreadable.
#
# That is the second half of what the operator reported: "the text font color was hard to see on
# the green background". A dark brand panel is fine where nothing is written, and fatal underneath
# a caption. So the layout follows WiX's own text positions rather than taste:
#
#   WelcomeDlg    Title/Description at X=135, width 220  -> the RIGHT of the dialog must be light
#   *Dlg banner   Title at X=15 W=200, Description X=25 W=340 -> the WHOLE banner must be light
#
# Hence: a narrow brand sidebar where no text is drawn, and a plain light field everywhere the
# wizard writes anything.
PANEL_TOP = (15, 31, 26)      # the brand sidebar (no text lands here)
PANEL_BOTTOM = (10, 21, 18)
PAGE_TOP = (250, 252, 251)    # the text field: near-white, so the system's dark text reads
PAGE_BOTTOM = (240, 244, 242)
ACCENT = (45, 140, 136)       # the mark's own teal, as a 2px rule
WORDMARK = (236, 245, 242)    # only used on the dark panel, where it is legible

# WHERE WIX STARTS WRITING. WelcomeDlg puts Title/Description at X=135, so everything from here
# right is the page; the sidebar is what is left.
TEXT_LEFT = 130

# WiX wants a real BMP. 24-bit RGB (no alpha channel): an alpha BMP is accepted by some
# tools and silently rendered with a black background by others, and the wizard
# background is not always white.
def save_bmp(im: Image.Image, path: pathlib.Path) -> None:
    im.convert("RGB").save(path, "BMP")
    print(f"  wrote {path.relative_to(REPO)}  {im.size[0]}x{im.size[1]}  {path.stat().st_size} bytes")


def vgradient(size: tuple[int, int], top: tuple[int, int, int],
               bottom: tuple[int, int, int]) -> Image.Image:
    """A vertical gradient, painted a row at a time — small enough to be free."""
    w, h = size
    im = Image.new("RGB", size, top)
    d = ImageDraw.Draw(im)
    for y in range(h):
        t = y / max(h - 1, 1)
        d.line(
            [(0, y), (w, y)],
            fill=(
                round(top[0] + (bottom[0] - top[0]) * t),
                round(top[1] + (bottom[1] - top[1]) * t),
                round(top[2] + (bottom[2] - top[2]) * t),
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
    """370 x 44, and LIGHT THROUGHOUT: the banner carries the page Title at X=15..215 and its
    Description at X=25..365, so there is no region of it free of text."""
    w, h = BANNER
    im = vgradient(BANNER, PAGE_TOP, PAGE_BOTTOM)
    # A brand rule along the bottom, 2px, which no text touches (Title is Y=6..21 of 44).
    ImageDraw.Draw(im).rectangle([(0, h - 2), (w, h - 1)], fill=ACCENT)
    # Small mark at the far right, clear of the Description's 25..365 span is impossible at this
    # width, so it goes in the corner ABOVE the rule and BELOW the text baseline: 30px tall in a
    # 44px strip leaves 7px top and bottom, and the Description starts at Y=23. Keep it out of the
    # way entirely instead — a banner with no logo, and the brand carried by the dialog panel.
    return im


def make_dialog() -> Image.Image:
    """370 x 234: brand sidebar on the left, plain light page on the right.

    WelcomeDlg and friends write their Title and Description at X=135, so the sidebar stops at
    130 and nothing of ours sits under their text."""
    w, h = DIALOG
    im = vgradient(DIALOG, PAGE_TOP, PAGE_BOTTOM)
    # The sidebar: dark, because nothing is written on it, so the light wordmark is legible.
    panel = vgradient((TEXT_LEFT, h), PANEL_TOP, PANEL_BOTTOM)
    im.paste(panel, (0, 0))
    ImageDraw.Draw(im).rectangle([(TEXT_LEFT - 2, 0), (TEXT_LEFT - 1, h)], fill=ACCENT)

    logo = scaled_icon(84)
    im.paste(logo, ((TEXT_LEFT - logo.width) // 2, 44), logo)

    font = load_font(20)
    if font is not None:
        d = ImageDraw.Draw(im)
        text = "Orchicon"
        tw = d.textlength(text, font=font)
        d.text(((TEXT_LEFT - tw) / 2, 44 + logo.height + 14), text, font=font, fill=WORDMARK)
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
