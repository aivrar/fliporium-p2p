"""Generate /og.png -- the 1200x630 social-preview card for fliporium.com."""

from PIL import Image, ImageDraw, ImageFont
import os

W, H = 1200, 630
BG = (19, 19, 24)               # #131318
INK = (241, 241, 255)
INK_MUTED = (154, 160, 173)
PURPLE = (201, 167, 255)
CREAM = (255, 245, 209)

ASSETS_DIR = os.path.dirname(os.path.abspath(__file__))

img = Image.new("RGB", (W, H), BG)

# Soft purple glow behind the logo via a separate RGBA layer composited back.
glow = Image.new("RGBA", (W, H), (0, 0, 0, 0))
gd = ImageDraw.Draw(glow)
# Radial-ish glow approximated with overlapping low-alpha ellipses.
for radius, alpha in [(420, 16), (320, 24), (220, 32), (140, 44)]:
    bbox = (220 - radius, 315 - radius, 220 + radius, 315 + radius)
    gd.ellipse(bbox, fill=(201, 167, 255, alpha))
img = Image.alpha_composite(img.convert("RGBA"), glow).convert("RGB")

d = ImageDraw.Draw(img)

# Logo image, center-left.
logo = Image.open(os.path.join(ASSETS_DIR, "icon-256.png")).convert("RGBA")
logo = logo.resize((260, 260), Image.LANCZOS)
img.paste(logo, (90, (H - 260) // 2), logo)

# Brand stripe accent (a thin cream bar above title for a circus-banner feel).
d.rectangle([(400, 175), (480, 183)], fill=CREAM)
d.rectangle([(495, 175), (575, 183)], fill=PURPLE)
d.rectangle([(590, 175), (670, 183)], fill=CREAM)

def load_font(names, size):
    for name in names:
        try:
            return ImageFont.truetype(name, size)
        except Exception:
            continue
    return ImageFont.load_default()

TITLE_FONTS = ["georgia.ttf", "georgiab.ttf", "calibri.ttf", "arial.ttf"]
BODY_FONTS = ["segoeui.ttf", "calibri.ttf", "arial.ttf"]

title_font = load_font(TITLE_FONTS, 120)
tagline_font = load_font(BODY_FONTS, 30)
url_font = load_font(BODY_FONTS, 22)

# Title.
title_x = 400
d.text((title_x, 200), "Fliporium", font=title_font, fill=INK)

# Tagline.
d.text((title_x, 360), "Friends, files, and side-by-side things,", font=tagline_font, fill=PURPLE)
d.text((title_x, 405), "over a private little tailnet.", font=tagline_font, fill=CREAM)

# Bottom URL strip.
d.text((title_x, H - 70), "fliporium.com", font=url_font, fill=INK_MUTED)

out = os.path.join(os.path.dirname(ASSETS_DIR), "site", "og.png")
img.save(out, "PNG", optimize=True)
print(f"wrote {out}")
