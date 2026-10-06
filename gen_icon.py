#!/usr/bin/env python3
"""生成 AGIEGG 鸡蛋图标。
- 参数化蛋形曲线（上窄下宽）+ 暖奶白渐变 + 左上柔光 + 顶部细裂纹 + 柔和投影
- 手写 ICO 容器（多尺寸 PNG 帧），避免 Pillow ICO 保存器的兼容问题
输出: build/windows/icon.ico 与 embedded/icon.png
"""
import io
import os
import struct
from PIL import Image, ImageDraw, ImageFilter, ImageChops

HERE = os.path.dirname(os.path.abspath(__file__))
OUT_ICO = os.path.join(HERE, "build", "windows", "icon.ico")
OUT_PNG = os.path.join(HERE, "embedded", "icon.png")

SS = 4  # 超采样倍率（抗锯齿）


def egg_polygon(size):
    """返回蛋形多边形顶点（在 size×size 画布内）。"""
    import math
    cx = size * 0.5
    a = size * 0.335          # 半宽
    b = size * 0.405          # 半高
    cy = size * 0.52          # 形心略偏下
    pts = []
    steps = 720
    for i in range(steps):
        t = 2 * math.pi * i / steps
        xx = math.cos(t)
        yy = math.sin(t)
        taper = 1.0 - 0.24 * (yy if yy > 0 else 0.0)   # 上半收窄 → 蛋尖
        X = cx + a * xx * taper
        Y = cy - b * yy
        pts.append((X, Y))
    return pts


def render(size):
    S = size * SS
    img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    poly = egg_polygon(S)

    # --- 投影 ---
    shadow = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    sd = ImageDraw.Draw(shadow)
    sd.polygon([(x, y + S * 0.03) for (x, y) in poly], fill=(60, 50, 30, 90))
    shadow = shadow.filter(ImageFilter.GaussianBlur(radius=S * 0.035))
    img = Image.alpha_composite(img, shadow)

    # --- 蛋体渐变色（竖直方向 奶白 → 暖金）---
    grad = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    gd = ImageDraw.Draw(grad)
    top_c = (255, 253, 246)
    bot_c = (240, 224, 186)
    for y in range(S):
        t = y / S
        c = tuple(int(top_c[i] + (bot_c[i] - top_c[i]) * t) for i in range(3))
        gd.line([(0, y), (S, y)], fill=c + (255,))
    # 用蛋形做蒙版
    mask = Image.new("L", (S, S), 0)
    ImageDraw.Draw(mask).polygon(poly, fill=255)
    eggan = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    eggan.paste(grad, (0, 0), mask)

    # --- 左上柔光高光 ---
    hl = Image.new("L", (S, S), 0)
    hd = ImageDraw.Draw(hl)
    hr = S * 0.17
    hcx = S * 0.34
    hcy = S * 0.30
    hd.ellipse([hcx - hr, hcy - hr, hcx + hr, hcy + hr], fill=150)
    hl = hl.filter(ImageFilter.GaussianBlur(radius=S * 0.05))
    hl = ImageChops.multiply(hl, mask)  # 限制在蛋内
    white = Image.new("RGBA", (S, S), (255, 255, 255, 255))
    eggan = Image.composite(white, eggan, hl)

    # --- 底部轻微暗部，增强体积 ---
    dk = Image.new("L", (S, S), 0)
    dd = ImageDraw.Draw(dk)
    dr = S * 0.30
    dd.ellipse([S * 0.5 - dr, S * 0.72, S * 0.5 + dr, S * 0.72 + dr * 1.1], fill=55)
    dk = dk.filter(ImageFilter.GaussianBlur(radius=S * 0.06))
    dk = ImageChops.multiply(dk, mask)
    dark = Image.new("RGBA", (S, S), (150, 130, 95, 255))
    eggan = Image.composite(dark, eggan, dk)

    img = Image.alpha_composite(img, eggan)

    # --- 顶部裂纹（破壳意象）---
    cd = ImageDraw.Draw(img)
    y0 = S * 0.235
    x0 = S * 0.40
    x1 = S * 0.60
    n = 6
    pts = []
    for i in range(n + 1):
        x = x0 + (x1 - x0) * i / n
        y = y0 + (S * 0.016 if i % 2 == 0 else -S * 0.016)
        pts.append((x, y))
    cd.line(pts, fill=(158, 132, 78, 235), width=max(1, int(S * 0.012)), joint="curve")

    # 降采样
    return img.resize((size, size), Image.LANCZOS)


def write_ico(path, images):
    """手写 ICO 容器：每个尺寸一张 PNG 帧。"""
    frames = []
    for im in images:
        buf = io.BytesIO()
        im.save(buf, format="PNG")
        frames.append(buf.getvalue())
    n = len(frames)
    header = struct.pack("<HHH", 0, 1, n)  # reserved, type=icon, count
    entries = b""
    offset = 6 + 16 * n
    datas = b""
    for im, data in zip(images, frames):
        w = 0 if im.width >= 256 else im.width
        h = 0 if im.height >= 256 else im.height
        entries += struct.pack("<BBBBHHII", w, h, 0, 0, 1, 32, len(data), offset)
        datas += data
        offset += len(data)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(header + entries + datas)


def main():
    sizes = [16, 24, 32, 48, 64, 128, 256]
    imgs = [render(s) for s in sizes]
    write_ico(OUT_ICO, imgs)
    os.makedirs(os.path.dirname(OUT_PNG), exist_ok=True)
    imgs[-1].save(OUT_PNG)
    print("wrote", OUT_ICO, "frames:", [i.size for i in imgs])
    print("wrote", OUT_PNG)


if __name__ == "__main__":
    main()
