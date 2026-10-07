# Cuts the web assets out of the Laurel Posts logo (see README, "The landing page").
# Usage: python3 make-assets.py LOGO.jpg OUTPUT_DIR
# Then quantize the PNGs: Image.open(f).quantize(256, method=Image.Quantize.FASTOCTREE).save(f, optimize=True)
import sys
from PIL import Image
src, out = sys.argv[1], sys.argv[2]
im = Image.open(src).convert('RGB')
BG = (248, 247, 243)
# Emblem only (signpost + wreath), with a little margin.
em = im.crop((856, 236, 1764, 940))
w, h = em.size
rgba = Image.new('RGBA', (w, h))
sp, dp = em.load(), rgba.load()
T = 46.0  # distance from the paper color at which a pixel is fully opaque
for y in range(h):
    for x in range(w):
        c = sp[x, y]
        d = max(abs(c[i] - BG[i]) for i in range(3))
        a = min(1.0, max(0.0, (d - 6) / T))
        if a <= 0:
            dp[x, y] = (0, 0, 0, 0)
            continue
        # Un-mix the paper from anti-aliased edge pixels.
        col = tuple(max(0, min(255, round((c[i] - (1 - a) * BG[i]) / a))) for i in range(3))
        dp[x, y] = col + (round(a * 255),)
bbox = rgba.getbbox()
rgba = rgba.crop(bbox)
print('emblem', rgba.size)
def fit(img, height):
    r = height / img.height
    return img.resize((max(1, round(img.width * r)), height), Image.LANCZOS)
fit(rgba, 480).save(f'{out}/emblem.png', optimize=True)        # hero, shown ~240px tall
fit(rgba, 96).save(f'{out}/emblem-sm.png', optimize=True)      # header, shown 40-48px
def square(img, size, pad, bg=None):
    canvas = Image.new('RGBA', (size, size), bg or (0, 0, 0, 0))
    inner = size - 2 * pad
    r = min(inner / img.width, inner / img.height)
    s = img.resize((round(img.width * r), round(img.height * r)), Image.LANCZOS)
    canvas.alpha_composite(s, ((size - s.width) // 2, (size - s.height) // 2))
    return canvas
square(rgba, 64, 2).save(f'{out}/favicon.png', optimize=True)
square(rgba, 180, 18, (248, 247, 243, 255)).convert('RGB').save(f'{out}/apple-touch-icon.png', optimize=True)
# Link-preview image for the landing page: the full logo, cropped to 1.91:1.
W, H = im.size
ch = round(W / 1.91)
og = im.crop((0, (H - ch) // 2, W, (H - ch) // 2 + ch)).resize((1200, 628), Image.LANCZOS)
og.save(f'{out}/logo-og.jpg', quality=86, optimize=True, progressive=True)
