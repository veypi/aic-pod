#!/usr/bin/env python3
"""ivec 品牌图标渲染器（统一风格，2026-11-05 定稿）。

统一规范（网页 / 桌面 / 商店 / 托盘 同一套）：
  - 形状：**满幅出血的 squircle**（超椭圆 n=5，macOS 图标惯例；macOS 不会自动给第三方图标加圆角）
  - 底色：**#f1f8ff**（浅底，不透明）
  - 标记：**纯黑 #000000** 描边（开口圆环 + A 字），占画布约 59~62%
  - 托盘：唯一保留透明底的场景（要叠在系统托盘上，macOS 走 trayTemplate 模板图自动反色）

几何与 aic/ui/assets/logo.svg 一致（环 R=77、stroke=12、A 的三段）。
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

MARK_SPAN = 0.5875                  # 原生几何的标记宽度占画布比例（46..187 / 240）
MARK_RATIO = 0.54                   # 桌面图标：标记占画布比例
FOOTPRINT = 0.84                    # 桌面图标：squircle 占画布比例（macOS 惯例约 0.82~0.85）
STORE_MARK_RATIO = 0.58             # 商店图标：满幅方块，标记占画布比例


def squircle_points(size, n=5.0, steps=192):
    """超椭圆（squircle）轮廓点，铺满画布。"""
    c = size / 2.0
    a = c
    pts = []
    for i in range(steps):
        t = 2 * math.pi * i / steps
        ct, st = math.cos(t), math.sin(t)
        x = a * math.copysign(abs(ct) ** (2.0 / n), ct)
        y = a * math.copysign(abs(st) ** (2.0 / n), st)
        pts.append((c + x, c + y))
    return pts


def squircle_svg_path(size=240, n=5.0, steps=96):
    """SVG path（满幅 squircle）。"""
    pts = squircle_points(size, n=n, steps=steps)
    d = "M" + " L".join(f"{x:.2f} {y:.2f}" for x, y in pts) + " Z"
    return d


def stroke_mask(size, color=255, stroke=STROKE):
    k = size / U
    m = Image.new("L", (size, size), 0)
    d = ImageDraw.Draw(m)
    w = max(1, int(round(stroke * k)))
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


def render(size, bg, mark_color=BLACK, mark_ratio=MARK_RATIO, shape="square",
           stroke=STROKE, n=5.0, footprint=1.0):
    """shape: square（商店，满幅方块） / squircle（桌面，squircle 按 footprint 留边距） / none（透明底）。"""
    base = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    d = ImageDraw.Draw(base)
    side = size * footprint
    off = (size - side) / 2.0
    if shape == "squircle":
        pts = squircle_points(int(side), n=n)
        d.polygon([(x + off, y + off) for x, y in pts], fill=bg)
    elif shape == "square":
        d.rectangle([0, 0, size - 1, size - 1], fill=bg)
    mask = stroke_mask(size, stroke=stroke)
    scale = (mark_ratio / MARK_SPAN) if mark_ratio else 1.0
    if scale != 1.0:
        ns = max(1, int(round(size * scale)))
        mask = mask.resize((ns, ns), Image.LANCZOS).resize((size, size), Image.LANCZOS)
    layer = Image.new("RGBA", (size, size), mark_color)
    layer.putalpha(mask)
    base.alpha_composite(layer)
    return base


def logo_svg(footprint=FOOTPRINT, mark_ratio=MARK_RATIO, n=5.0, steps=96):
    """网页 logo.svg：squircle 底（占画布 footprint）+ 标记（占画布 mark_ratio），与图标同规格。"""
    k = mark_ratio / MARK_SPAN
    cx, cy = 116.5, 120.0            # 原生标记的几何中心
    tx = 120.0 - cx * k
    ty = 120.0 - cy * k
    side = 240.0 * footprint
    off = (240.0 - side) / 2.0
    pts = squircle_points(int(side), n=n, steps=steps)
    path = "M" + " L".join(f"{x + off:.2f} {y + off:.2f}" for x, y in pts) + " Z"
    return f'''<svg width="240" height="240" viewBox="0 0 240 240" fill="none" xmlns="http://www.w3.org/2000/svg">
  <!-- 平台 logo 唯一真源：squircle 底 #f1f8ff（占画布 {footprint:.0%}，macOS 图标惯例）+ 黑色标记（占 {mark_ratio:.0%}），不透明。
       favicon / og:image / 分享卡 / 网页 header / dock / 桌面应用图标 / 商店图标 全部同一套风格。
       暗色主题：页面 CSS filter: invert(1) 整体反色 → 深底白标。 -->
  <path d="{path}" fill="#F1F8FF"/>
  <g transform="translate({tx:.2f} {ty:.2f}) scale({k:.4f})">
    <path
      d="M187 76C173.5 57.3333 150 46 123 46C79.3695 46 46 79.5786 46 120C46 160.421 79.3695 194 123 194C150 194 173.5 182.667 187 164"
      stroke="#000000" stroke-width="12" stroke-linecap="round" stroke-linejoin="round"/>
    <path d="M84 174L120 70L156 174" stroke="#000000" stroke-width="12" stroke-linecap="round"
      stroke-linejoin="round"/>
    <path d="M95 138H145" stroke="#000000" stroke-width="12" stroke-linecap="round"
      stroke-linejoin="round"/>
  </g>
</svg>
'''


LOGO_SVG = '''<svg width="240" height="240" viewBox="0 0 240 240" fill="none" xmlns="http://www.w3.org/2000/svg">
  <!-- 平台 logo 唯一真源：满幅 squircle 底 #f1f8ff + 黑色标记，不透明。
       favicon / og:image / 分享卡 / 网页 header / dock / 桌面应用图标 / 商店图标 全部同一套风格。
       暗色主题：页面 CSS filter: invert(1) 整体反色 → 深底白标。 -->
  <path d="{SQUIRCLE}" fill="#F1F8FF"/>
  <path
    d="M187 76C173.5 57.3333 150 46 123 46C79.3695 46 46 79.5786 46 120C46 160.421 79.3695 194 123 194C150 194 173.5 182.667 187 164"
    stroke="#000000" stroke-width="12" stroke-linecap="round" stroke-linejoin="round"/>
  <path d="M84 174L120 70L156 174" stroke="#000000" stroke-width="12" stroke-linecap="round"
    stroke-linejoin="round"/>
  <path d="M95 138H145" stroke="#000000" stroke-width="12" stroke-linecap="round"
    stroke-linejoin="round"/>
</svg>
'''


def main():
    out = sys.argv[1] if len(sys.argv) > 1 else os.path.dirname(os.path.abspath(__file__))
    os.makedirs(out, exist_ok=True)

    # 桌面端：squircle（占画布 FOOTPRINT）+ 黑标（占画布 MARK_RATIO）—— 与网页 logo.svg 同一规格
    for s in (1024, 512, 256):
        render(s, FIELD, shape="squircle", footprint=FOOTPRINT,
               mark_ratio=MARK_RATIO).save(os.path.join(out, f"ivec-desktop-icon-{s}.png"))
    # 商店图标：满幅方块（商店自己加圆角）
    for s in (1024, 512, 256, 216, 180, 152, 120, 76, 60, 48, 32, 16):
        render(s, FIELD, shape="square", mark_ratio=STORE_MARK_RATIO).convert("RGB").save(
            os.path.join(out, f"ivec-app-icon-{s}.png"))
    # Android 自适应前景层（透明底 + 黑标，图形收 66% 安全区）
    for s in (432, 1024):
        render(s, CLEAR, mark_ratio=0.42, shape="none").save(
            os.path.join(out, f"ivec-adaptive-fg-{s}.png"))
    # 透明底标记 + 托盘（没有 squircle 包裹，标记要放大到 ~86%）
    for s in (1024, 512, 256, 128):
        render(s, CLEAR, mark_ratio=0.86, shape="none").save(os.path.join(out, f"ivec-mark-{s}.png"))
    render(128, CLEAR, mark_ratio=0.86, shape="none").save(os.path.join(out, "ivec-tray-128.png"))
    # 网页 logo.svg（squircle 底 + 同比例标记）
    with open(os.path.join(out, "logo.svg"), "w") as f:
        f.write(logo_svg())

    # 联络表
    pad, cell = 22, 170
    sheet = Image.new("RGB", (pad * 4 + cell * 3, pad * 4 + cell + 150), (233, 230, 225))
    sd = ImageDraw.Draw(sheet)
    im = Image.open(os.path.join(out, "ivec-desktop-icon-1024.png")).resize((cell, cell), Image.LANCZOS)
    sheet.paste(im, (pad, pad), im)
    sd.text((pad, pad + cell + 6), "desktop (squircle, full bleed)", fill=(40, 40, 40))
    im = Image.open(os.path.join(out, "ivec-app-icon-1024.png")).resize((cell, cell), Image.LANCZOS)
    sheet.paste(im, (pad * 2 + cell, pad))
    sd.text((pad * 2 + cell, pad + cell + 6), "store (square, full bleed)", fill=(40, 40, 40))
    im = Image.open(os.path.join(out, "ivec-mark-512.png")).resize((cell, cell), Image.LANCZOS)
    sheet.paste(im, (pad * 3 + cell * 2, pad), im)
    sd.text((pad * 3 + cell * 2, pad + cell + 6), "mark (transparent)", fill=(40, 40, 40))
    x = pad
    y = pad * 3 + cell
    for s in (216, 120, 60, 32, 16):
        sheet.paste(Image.open(os.path.join(out, f"ivec-app-icon-{s}.png")), (x, y))
        sd.text((x, y + s + 4), str(s), fill=(60, 60, 60))
        x += max(s, 40) + 18
    x = pad * 2 + cell
    for s in (128,):
        sheet.paste(Image.open(os.path.join(out, "ivec-mark-128.png")), (x, y))
        sd.text((x, y + s + 4), "mark 128", fill=(60, 60, 60))
        x += max(s, 40) + 18
    sheet.save(os.path.join(out, "ivec-app-icon-sheet.png"))
    print("OK ->", out)


if __name__ == "__main__":
    main()
