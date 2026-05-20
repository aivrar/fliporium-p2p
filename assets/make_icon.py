"""Generate the Fliporium icon family: SVG (web), multi-size PNGs, and .ico
(Windows). Renders a stylized circus tent in the Fliporium purple, matching
the in-app vocabulary (Marquee, Booth, Floor) and tour iconography.

Design rules:
  - Readable down to 16x16 (taskbar), so silhouette must be obvious without
    interior detail. The interior detail (stripe-rays, banner, entrance)
    only appears at >=48px and gets progressively richer up to 256px.
  - Transparent background; equally legible on light and dark OS themes.
  - Brand purple #c9a7ff as the primary stripe, warm cream #fff5d1 as the
    secondary stripe, accent green #4ade80 on the flag.
"""

from PIL import Image, ImageDraw
import math
import os

PURPLE = (201, 167, 255, 255)   # #c9a7ff -- Fliporium primary
PURPLE_DARK = (91, 58, 163, 255) # #5b3aa3 -- pole, outline
CREAM = (255, 245, 209, 255)    # #fff5d1 -- secondary stripe
GREEN = (74, 222, 128, 255)     # #4ade80 -- connection-OK accent, flag
SHADOW = (40, 30, 70, 60)       # subtle drop shadow

ASSETS_DIR = os.path.dirname(os.path.abspath(__file__))


def _scale(p, size):
    """Convert a (x,y) coord in 256-space to target size."""
    return (p[0] * size / 256.0, p[1] * size / 256.0)


def draw_tent(size, detail="full"):
    """Render at given size. detail='silhouette' for tiny sizes (<=24),
    'medium' for 32-48, 'full' for 64+."""
    img = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    d = ImageDraw.Draw(img, "RGBA")

    # All geometry expressed in a 256-unit grid then scaled.
    s = size / 256.0

    # ---- Tent footprint ----
    # Peak at (128, 60). Base at y=210. Slight outward bulge in the walls.
    peak = (128, 60)
    base_left = (32, 210)
    base_right = (224, 210)

    # ---- Soft drop shadow (full detail only) ----
    if detail == "full" and size >= 96:
        shadow_poly = [
            (base_left[0] - 6, base_left[1] + 10),
            (base_right[0] + 6, base_right[1] + 10),
            (base_right[0] - 4, base_right[1] + 18),
            (base_left[0] + 4, base_left[1] + 18),
        ]
        d.polygon([(_scale(p, size)) for p in shadow_poly], fill=SHADOW)

    # ---- Stripe rays from peak ----
    # For full detail: 8 stripes alternating purple/cream.
    # For medium: 4 stripes.
    # For silhouette: 2 wedges (solid purple tent + cream entrance).
    if detail == "silhouette":
        # Single solid tent silhouette in purple.
        tent_outline = [peak, base_right, base_left]
        d.polygon([_scale(p, size) for p in tent_outline], fill=PURPLE)
    else:
        n_stripes = 8 if detail == "full" else 4
        # Base edge runs from base_left to base_right; we divide it into
        # n_stripes wedges, each anchored at the peak.
        for i in range(n_stripes):
            t0 = i / n_stripes
            t1 = (i + 1) / n_stripes
            x0 = base_left[0] + (base_right[0] - base_left[0]) * t0
            x1 = base_left[0] + (base_right[0] - base_left[0]) * t1
            color = PURPLE if i % 2 == 0 else CREAM
            wedge = [peak, (x0, base_right[1]), (x1, base_right[1])]
            d.polygon([_scale(p, size) for p in wedge], fill=color)

    # ---- Tent scalloped roof brim (full only) ----
    # A small ellipse-arc at each base corner gives the classic "tent skirt"
    # silhouette. Skip on tiny sizes.
    if detail == "full" and size >= 64:
        brim_h = 14
        brim_color = PURPLE_DARK
        d.polygon(
            [
                _scale((base_left[0], base_left[1]), size),
                _scale((base_right[0], base_right[1]), size),
                _scale((base_right[0] - 6, base_right[1] + brim_h), size),
                _scale((base_left[0] + 6, base_left[1] + brim_h), size),
            ],
            fill=brim_color,
        )

    # ---- Outline along the tent edges (full only) ----
    if detail == "full" and size >= 48:
        outline_w = max(2, int(2 * s))
        d.line(
            [_scale(peak, size), _scale(base_left, size)],
            fill=PURPLE_DARK,
            width=outline_w,
        )
        d.line(
            [_scale(peak, size), _scale(base_right, size)],
            fill=PURPLE_DARK,
            width=outline_w,
        )

    # ---- Entrance arch (medium+) ----
    # A small dark arch at the base center -- the "doorway" into the booth.
    if detail != "silhouette" and size >= 32:
        arch_w, arch_h = 38, 50
        ax = peak[0] - arch_w / 2
        ay = base_right[1] - arch_h
        # Filled chord
        bbox = [_scale((ax, ay), size), _scale((ax + arch_w, ay + arch_h * 2), size)]
        d.pieslice(bbox, start=180, end=360, fill=PURPLE_DARK)

    # ---- Flag pole + pennant (medium+) ----
    if detail != "silhouette" and size >= 32:
        pole_top = (peak[0], peak[1] - 36)
        pole_w = max(2, int(3 * s))
        d.line(
            [_scale(pole_top, size), _scale(peak, size)],
            fill=PURPLE_DARK,
            width=pole_w,
        )
        # Pennant flag triangle, points to the right.
        flag = [
            pole_top,
            (pole_top[0] + 22, pole_top[1] + 6),
            (pole_top[0], pole_top[1] + 14),
        ]
        d.polygon([_scale(p, size) for p in flag], fill=GREEN)

    # ---- Top ball cap (full only) ----
    if detail == "full" and size >= 64:
        cap_r = 5
        bbox = [
            _scale((peak[0] - cap_r, peak[1] - cap_r), size),
            _scale((peak[0] + cap_r, peak[1] + cap_r), size),
        ]
        d.ellipse(bbox, fill=PURPLE_DARK)

    return img


def main():
    sizes_full = [256, 128, 96, 64]
    sizes_medium = [48, 32]
    sizes_silhouette = [24, 16]

    pngs = {}
    for sz in sizes_full:
        pngs[sz] = draw_tent(sz, "full")
    for sz in sizes_medium:
        pngs[sz] = draw_tent(sz, "medium")
    for sz in sizes_silhouette:
        pngs[sz] = draw_tent(sz, "silhouette")

    # Write per-size PNGs.
    for sz, img in pngs.items():
        out = os.path.join(ASSETS_DIR, f"icon-{sz}.png")
        img.save(out, "PNG")
        print(f"wrote {out}")

    # Bundle .ico from a curated subset (Windows convention).
    ico_sizes = [16, 24, 32, 48, 64, 128, 256]
    ico_imgs = [pngs[sz] for sz in ico_sizes]
    ico_path = os.path.join(ASSETS_DIR, "fliporium.ico")
    ico_imgs[0].save(
        ico_path,
        format="ICO",
        sizes=[(sz, sz) for sz in ico_sizes],
        append_images=ico_imgs[1:],
    )
    print(f"wrote {ico_path}")

    # Also a 512px hero for og:image, social cards, etc.
    big = draw_tent(512, "full")
    big.save(os.path.join(ASSETS_DIR, "icon-512.png"))
    print("wrote icon-512.png")


if __name__ == "__main__":
    main()
