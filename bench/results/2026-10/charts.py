#!/usr/bin/env python3
"""生成 README 性能小节用的图表：assets/bench/<名字>-{light,dark}.svg。

数字取自本目录 README.md 的结果表（中位数），改了表就改这里，再运行：
    python3 bench/results/2026-10/charts.py
只依赖标准库。
"""

import os

OUT = os.path.join(os.path.dirname(__file__), "..", "..", "..", "assets", "bench")

TOOLS = ["local-mirror", "rsync", "Syncthing"]

# 配色：参考调色板分类色前三位（蓝、橙、青绿），已对 GitHub 亮/暗背景校验
THEMES = {
    "light": dict(series=["#2a78d6", "#eb6834", "#1baf7a"], ink="#1f2328", muted="#59636e",
                  grid="#e1e0d9", axis="#c3c2b7", surface="#ffffff", old="#9e9c95"),
    "dark": dict(series=["#3987e5", "#d95926", "#199e70"], ink="#f0f6fc", muted="#9198a1",
                 grid="#2c2c2a", axis="#383835", surface="#0d1117", old="#6e6c66"),
}

FONT = "-apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif"

# 每张图：分组标签 + 每个工具的值（None = 未测）
LATENCY = [
    ("tiny · loopback", [1.44, None, 2.31]),
    ("small · loopback", [1.44, 0.60, 2.32]),
    ("small · Mac → Debian", [1.51, 3.26, 2.33]),
    ("small · Debian → Mac", [1.51, 2.85, 2.30]),
    ("mixed · Mac → Debian", [1.51, 1.06, 2.31]),
    ("mixed · Debian → Mac", [1.48, 1.00, 2.29]),
]
CHANGE_CPU = [
    ("small · loopback", [0.01, 0.97, 0.26]),
    ("small · Mac → Debian", [0.01, 1.76, 0.51]),
    ("small · Debian → Mac", [0.01, 1.42, 0.32]),
    ("mixed · Mac → Debian", [0.01, 0.43, 0.11]),
    ("mixed · Debian → Mac", [0.01, 0.36, 0.09]),
]
CREATE = [
    ("tiny · loopback", [1.99, 0.35, 6.40]),
    ("small · loopback", [2.18, 0.90, 6.57]),
    ("small · Mac → Debian", [2.05, 3.84, 4.45]),
    ("small · Debian → Mac", [2.64, 3.48, 7.29]),
    ("mixed · Mac → Debian", [2.60, 1.90, 10.7]),
    ("mixed · Debian → Mac", [2.35, 1.78, 6.98]),
]
DELETE = [
    ("tiny · loopback", [1.52, 0.15, 2.19]),
    ("small · loopback", [1.52, 0.70, 7.55]),
    ("small · Mac → Debian", [1.17, 3.38, 7.07]),
    ("small · Debian → Mac", [1.87, 2.96, 13.98]),
    ("mixed · Mac → Debian", [1.58, 1.14, 3.97]),
    ("mixed · Debian → Mac", [1.75, 1.03, 5.59]),
]
FIRST_TIME = [
    ("small · loopback", [41.1, 21.5, 726]),
    ("small · Mac → Debian", [35.0, 13.0, 297]),
    ("small · Debian → Mac", [40.5, 23.0, 752]),
    ("mixed · Mac → Debian", [22.7, 23.3, 93.0]),
    ("mixed · Debian → Mac", [22.7, 22.2, 144]),
    ("large · Debian → Mac", [59.3, 60.4, 62.5]),
]
FIRST_CPU = [
    ("small · loopback", [63.3, 29.0, 327]),
    ("small · Mac → Debian", [67.1, 15.5, 446]),
    ("small · Debian → Mac", [72.2, 26.3, 439]),
    ("mixed · Mac → Debian", [32.2, 14.6, 156]),
    ("mixed · Debian → Mac", [35.3, 13.0, 100]),
    ("large · Debian → Mac", [61.2, 27.5, 43.8]),
]

LABEL_W = 170  # 左侧分组标签列
BAR_H = 10
BAR_GAP = 2
GROUP_GAP = 16
VALUE_W = 54  # 数值标签预留


def fmt(v, unit):
    if v is None:
        return "not measured"
    if unit == "cpu-change" and v <= 0.01:
        return "≤ 0.01 s"
    # 与结果表同精度：10 以下两位小数，10–100 至少一位，100 以上取整
    if v >= 100:
        return f"{v:.0f} s"
    s = f"{v:.2f}"
    if v >= 10 and s.endswith("0"):
        s = s[:-1]
    return s + " s"


def bar_path(x, y, w, h):
    """左端贴基线为直角、数据端为 4px 圆角的横条"""
    r = min(4, h / 2, w)
    if w <= 0:
        return ""
    return (f"M{x:.1f},{y:.1f}h{w - r:.1f}a{r},{r} 0 0 1 {r},{r}v{h - 2 * r:.1f}"
            f"a{r},{r} 0 0 1 {-r},{r}h{-(w - r):.1f}z")


def ticks(cap):
    for step in (0.25, 0.5, 1, 2, 2.5, 5, 10, 20, 25, 40, 50, 100):
        if cap / step <= 5:
            n = int(cap / step + 1e-9)
            return [i * step for i in range(n + 1)]
    return [0, cap]


def panel(t, x0, y0, width, groups, cap, unit, show_labels):
    """一个横向分组柱状图面板，返回 (svg 片段, 高度)"""
    out = []
    plot_w = width - VALUE_W
    group_h = 3 * BAR_H + 2 * BAR_GAP
    h = len(groups) * (group_h + GROUP_GAP)
    sx = lambda v: x0 + plot_w * min(v, cap) / cap
    tick_vals = ticks(cap)
    for tv in tick_vals:
        x = sx(tv)
        out.append(f'<line x1="{x:.1f}" y1="{y0 - 4}" x2="{x:.1f}" y2="{y0 + h - GROUP_GAP + 4}" '
                   f'stroke="{t["grid"]}" stroke-width="1"/>')
        lab = f"{tv:g}"
        out.append(f'<text x="{x:.1f}" y="{y0 + h - GROUP_GAP + 18}" fill="{t["muted"]}" '
                   f'font-size="11" text-anchor="middle">{lab}</text>')
    out.append(f'<line x1="{x0}" y1="{y0 - 4}" x2="{x0}" y2="{y0 + h - GROUP_GAP + 4}" '
               f'stroke="{t["axis"]}" stroke-width="1"/>')
    for gi, (label, vals) in enumerate(groups):
        gy = y0 + gi * (group_h + GROUP_GAP)
        if show_labels:
            out.append(f'<text x="{LABEL_W - 12}" y="{gy + group_h / 2 + 4:.1f}" fill="{t["ink"]}" '
                       f'font-size="12" text-anchor="end">{label}</text>')
        for si, v in enumerate(vals):
            by = gy + si * (BAR_H + BAR_GAP)
            if v is None:
                out.append(f'<text x="{x0 + 4}" y="{by + BAR_H - 1}" fill="{t["muted"]}" '
                           f'font-size="10.5" font-style="italic">{fmt(v, unit)}</text>')
                continue
            w = max(sx(v) - x0, 1.5)
            out.append(f'<path d="{bar_path(x0, by, w, BAR_H)}" fill="{t["series"][si]}"/>')
            if v > cap:
                # 截断标记：两道与背景同色的斜线
                bx = x0 + w - 14
                for dx in (0, 5):
                    out.append(f'<line x1="{bx + dx:.1f}" y1="{by + BAR_H + 1}" x2="{bx + dx + 4:.1f}" '
                               f'y2="{by - 1}" stroke="{t["surface"]}" stroke-width="2"/>')
            out.append(f'<text x="{x0 + w + 5:.1f}" y="{by + BAR_H - 1}" fill="{t["ink"]}" '
                       f'font-size="10.5">{fmt(v, unit)}</text>')
    return "\n".join(out), h - GROUP_GAP + 24


def legend(t, x, y, items, colors):
    out, cx = [], x
    for name, c in zip(items, colors):
        out.append(f'<rect x="{cx}" y="{y - 9}" width="10" height="10" rx="2" fill="{c}"/>')
        out.append(f'<text x="{cx + 15}" y="{y}" fill="{t["ink"]}" font-size="12">{name}</text>')
        cx += 15 + 7.2 * len(name) + 22
    return "\n".join(out)


def chart(name, panels, width=760, axis_note="seconds"):
    """panels: [(子标题, groups, cap, unit)]，多个面板横向并排、共用左侧分组标签"""
    for mode, t in THEMES.items():
        body = [legend(t, LABEL_W, 16, TOOLS, t["series"])]
        top = 44 if len(panels) > 1 else 34
        avail = width - LABEL_W - 10
        pw = (avail - 24 * (len(panels) - 1)) / len(panels)
        height = 0
        for pi, (sub, groups, cap, unit) in enumerate(panels):
            x0 = LABEL_W + pi * (pw + 24)
            if sub:
                body.append(f'<text x="{x0}" y="{top - 10}" fill="{t["muted"]}" font-size="12" '
                            f'font-weight="600">{sub}</text>')
            frag, h = panel(t, x0, top + 4, pw, groups, cap, unit, pi == 0)
            body.append(frag)
            height = max(height, top + 4 + h)
        body.append(f'<text x="{width - 10}" y="{height + 14}" fill="{t["muted"]}" font-size="11" '
                    f'text-anchor="end">{axis_note}</text>')
        svg = (f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height + 22}" '
               f'viewBox="0 0 {width} {height + 22}" font-family="{FONT}">\n' + "\n".join(body) + "\n</svg>\n")
        with open(os.path.join(OUT, f"{name}-{mode}.svg"), "w") as f:
            f.write(svg)


def versions_chart():
    """v2.12.1 首次同步前后：同一指标两个版本，旧版本用中性灰"""
    rows = [("Time", 40.8, 30.8), ("CPU", 65.3, 40.2)]
    width, x0, plot_w = 760, LABEL_W, 760 - LABEL_W - 10 - VALUE_W
    for mode, t in THEMES.items():
        colors = [t["old"], t["series"][0]]
        body = [legend(t, LABEL_W, 16, ["v2.12.0", "v2.12.1"], colors)]
        y0, cap = 38, 70
        sx = lambda v: plot_w * v / cap
        group_h = 2 * 14 + 2
        for tv in ticks(cap):
            x = x0 + sx(tv)
            body.append(f'<line x1="{x:.1f}" y1="{y0 - 4}" x2="{x:.1f}" y2="{y0 + 2 * (group_h + GROUP_GAP) - GROUP_GAP + 4}" '
                        f'stroke="{t["grid"]}" stroke-width="1"/>')
            body.append(f'<text x="{x:.1f}" y="{y0 + 2 * (group_h + GROUP_GAP) - GROUP_GAP + 18}" fill="{t["muted"]}" '
                        f'font-size="11" text-anchor="middle">{tv:g}</text>')
        for gi, (label, old, new) in enumerate(rows):
            gy = y0 + gi * (group_h + GROUP_GAP)
            body.append(f'<text x="{LABEL_W - 12}" y="{gy + group_h / 2 + 4:.1f}" fill="{t["ink"]}" '
                        f'font-size="12" text-anchor="end">{label}</text>')
            for si, v in enumerate((old, new)):
                by = gy + si * 16
                body.append(f'<path d="{bar_path(x0, by, sx(v), 14)}" fill="{colors[si]}"/>')
                body.append(f'<text x="{x0 + sx(v) + 5:.1f}" y="{by + 11}" fill="{t["ink"]}" font-size="11">{v:.1f} s</text>')
        body.append(f'<line x1="{x0}" y1="{y0 - 4}" x2="{x0}" y2="{y0 + 2 * (group_h + GROUP_GAP) - GROUP_GAP + 4}" '
                    f'stroke="{t["axis"]}" stroke-width="1"/>')
        height = y0 + 2 * (group_h + GROUP_GAP) - GROUP_GAP + 38
        body.append(f'<text x="{width - 10}" y="{height}" fill="{t["muted"]}" font-size="11" text-anchor="end">seconds</text>')
        svg = (f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height + 8}" '
               f'viewBox="0 0 {width} {height + 8}" font-family="{FONT}">\n' + "\n".join(body) + "\n</svg>\n")
        with open(os.path.join(OUT, f"first-sync-v2.12.1-{mode}.svg"), "w") as f:
            f.write(svg)


if __name__ == "__main__":
    os.makedirs(OUT, exist_ok=True)
    chart("change-latency", [(None, LATENCY, 3.5, "s")])
    chart("change-cpu", [(None, CHANGE_CPU, 2, "cpu-change")], axis_note="CPU seconds, both ends")
    chart("burst", [("Create 1,000 files", CREATE, 15, "s"), ("Delete 1,000 files", DELETE, 15, "s")])
    chart("first-sync", [("Time", FIRST_TIME, 160, "s"), ("CPU, both ends", FIRST_CPU, 160, "s")],
          axis_note="seconds · bars past 160 s are cut, labels show the full value")
    versions_chart()
