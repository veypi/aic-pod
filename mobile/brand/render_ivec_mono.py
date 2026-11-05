#!/usr/bin/env python3
"""ivec 品牌图标渲染器（单色系：黑描边 + #f1f8ff 底）。

几何 = aic/ui/assets/logo.svg（开口圆环 + A 字，stroke-width 12，viewBox 240）。
logo.svg 本体是**黑色描边 + 透明底**：网页暗色主题靠 CSS `filter: invert(1)` 反色；
应用图标这类无法用 CSS 的场景，用 #f1f8ff 作底色 + 黑色标记。

输出：
  ivec-mark-*.png            透明底 + 黑标记（网页/托盘/反色底图）
  ivec-app-icon-*.png        满幅 #f1f8ff 底 + 黑标记（商店，不自带圆角）
  ivec-desktop-icon-*.png    透明底 + 圆角 #f1f8ff 方块 + 黑标记（桌面端应用图标）
  ivec-adaptive-fg-*.png     透明底 + 黑标记（Android 自适应前景层，图形收 66% 安全区）
  ivec-tray-128.png          透明底 + 黑标记（托盘）
用法: python3 render_ivec_mono.py [输出目录]
"""
import math
import os
import sys

from PIL import Image, ImageDraw

U = 240.0
RING_C = (123.0, 120.0)
RING_R = 77.0
RING_A0 = 34.5
RING_A1 = 325.5
STROKE = 12.0

FIELD = (0xF1, 0xF8, 0xFF, 255)     # 品牌底色 #f1f8ff
BLACK = (0, 0, 0, 255)              # 标记颜色
CLEAR = (0, 0, 0, 0)


def stroke_mask(size, color=255):
    k = size / U
    m = Image.new("L", (size, size), 0)
    d = ImageDraw.Draw(m)
    w = max(1, int(round(STROKE * k)))
    r = w / 2.0
    cx, cy = RING_C[0] * k, RING_C[1] * k
    rad = RING_R * k
    d.arc([cx - rad, cy - rad, cx + rad, cy + rad], RING_A0, RING_A1, fill=color, width=w)
    for ang in (RING_A0, RING_A1):
        a = math.radians(ang)
        x = cx + rad * math.cos(a)
        y = cy + rad * math.sin(a)
        d.ellipse([x - r, y - r, x + r, y + r], fill=color)
    segs = [((84, 174), (120, 70)), ((120, 70), (156, 174)), ((95, 138), (145, 138))]
    for (x1, y1), (x2, y2) in segs:
        d.line([(x1 * k, y1 * k), (x2 * k, y2 * k)], fill=color, width=w)
    for (x, y) in [(84, 174), (120, 70), (156, 174), (95, 138), (145, 138)]:
        d.ellipse([x * k - r, y * k - r, x * k + r, y * k + r], fill=color)
    return m


def render(size, bg, mark_scale=1.0, rounded=False, inset=0.0, radius_ratio=0.22):
    """bg=CLEAR 且 rounded=True 时画圆角底色方块。"""
    base = Image.new("RGBA", (size, size), bg)
    if rounded and bg[3] > 0:
        pad = int(round(size * inset))
        side = size - 2 * pad
        r = int(round(side * radius_ratio))
        ImageDraw.Draw(base).rounded_rectangle([pad, pad, size - pad, size - pad], radius=r, fill=bg)
    mask = stroke_mask(size)
    if mark_scale != 1.0:
        ns = int(round(size * mark_scale))
        mask = mask.resize((ns, ns), Image.LANCZOS).resize((size, size), Image.LANCZOS)
    layer = Image.new("RGBA", (size, size), BLACK)
    layer.putalpha(mask)
    base.alpha_composite(layer)
    return base


def main():
    out = sys.argv[1] if len(sys.argv) > 1 else os.path.dirname(os.path.abspath(__file__))
    os.makedirs(out, exist_ok=True)

    # 透明底标记
    for s in (1024, 512, 256, 128):
        render(s, CLEAR, mark_scale=0.92).save(os.path.join(out, f"ivec-mark-{s}.png"))
    # 商店图标：满幅 #f1f8ff
    for s in (1024, 512, 256, 216, 180, 152, 120, 76, 60, 48, 32, 16):
        render(s, FIELD, mark_scale=0.72).convert("RGB").save(
            os.path.join(out, f"ivec-app-icon-{s}.png"))
    # 桌面端应用图标：透明底 + 圆角 #f1f8ff 方块
    for s in (1024, 512, 256):
        render(s, FIELD, mark_scale=0.56, rounded=True, inset=0.055).save(
            os.path.join(out, f"ivec-desktop-icon-{s}.png"))
    # Android 自适应前景层
    for s in (432, 1024):
        render(s, CLEAR, mark_scale=0.66).save(os.path.join(out, f"ivec-adaptive-fg-{s}.png"))
    # 托盘
    render(128, CLEAR, mark_scale=0.92).save(os.path.join(out, "ivec-tray-128.png"))

    # 联络表
    pad, cell = 24, 200
    sheet = Image.new("RGB", (pad * 3 + cell * 3, pad * 3 + cell + 150), (233, 230, 225))
    sd = ImageDraw.Draw(sheet)
    im = Image.open(os.path.join(out, "ivec-app-icon-1024.png")).resize((cell, cell), Image.LANCZOS)
    sheet.paste(im, (pad, pad)); sd.text((pad, pad + cell + 6), "app-icon-1024 (#f1f8ff bg)", fill=(40, 40, 40))
    im = Image.open(os.path.join(out, "ivec-desktop-icon-1024.png")).resize((cell, cell), Image.LANCZOS)
    sheet.paste(im, (pad * 2 + cell, pad), im); sd.text((pad * 2 + cell, pad + cell + 6), "desktop-1024 (rounded)", fill=(40, 40, 40))
    im = Image.open(os.path.join(out, "ivec-mark-512.png")).resize((cell, cell), Image.LANCZOS)
    sheet.paste(im, (pad * 3 + cell * 2, pad), im); sd.text((pad * 3 + cell * 2, pad + cell + 6), "mark (transparent, web)", fill=(40, 40, 40))
    y = pad * 3 + cell
    x = pad
    for s in (216, 120, 60, 32, 16):
        sheet.paste(Image.open(os.path.join(out, f"ivec-app-icon-{s}.png")), (x, y))
        sd.text((x, y + s + 4), str(s), fill=(60, 60, 60))
        x += max(s, 40) + 18
    sheet.save(os.path.join(out, "ivec-app-icon-sheet.png"))
    print("OK ->", out)


if __name__ == "__main__":
    main()
