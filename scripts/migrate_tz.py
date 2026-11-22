#!/usr/bin/env python3
"""把 usage.json / channel-stats.json 的小时桶键从 UTC 平移到 UTC+8。

## 为什么需要它

这两个账本的桶键是「不带时区的本地时间字符串」：

- `usage.json`：`"s": "h:2026-11-22T04"`
- `channel-stats.json`：`"s": "h:2026-11-22T04"`

键里的时刻按**进程当时的 `time.Local`** 写入。如果宿主系统时区是 `Etc/UTC`、
服务也没设 `TZ`，这些键记的就是 UTC 时刻；之后给服务加上 `TZ=Asia/Shanghai`，
程序会把这些键按北京时间重新解释 —— 同一个键代表绝对时刻整体前移 8 小时，
历史曲线与「今天」的边界都会错位。

本脚本把存量键 +8 小时重写：**绝对时刻不变**，只是换成新时区下的正确表示。

## 前置条件（脚本会自己检查）

- 只处理小时桶（`h:`）。遇到日桶（`d:`）直接退出：日桶横跨两个时区日，
  不能平移，必须拿小时数据按新时区重新折叠。本仓库小时桶保留 90 天，
  正常数据里不会出现日桶。
- `saved` / `since` / `last_at` / `learned_total` 等元信息**保持不动**：
  它们要么是 RFC3339 绝对时刻（带 `Z` 或偏移，与时区无关），要么是纯计数。

## 用法

在**停服状态**下执行（运行中的进程会把内存快照写回，覆盖你的迁移）：

    sudo systemctl stop workbuddy-panel
    python3 scripts/migrate_tz.py ~/workbuddy2api-panel/data              # 演练
    python3 scripts/migrate_tz.py ~/workbuddy2api-panel/data --apply      # 应用
    sudo systemctl start workbuddy-panel

演练模式只写 `*.json.new`，不碰原文件；应用模式先写临时文件再 `os.replace()`
原子替换，中途失败不会留下半个文件。

## 验证

脚本会打印迁移前后的桶数、事件总数/用量合计，任何一项不等就直接退出（不写文件）。
"""
import argparse
import datetime
import json
import os
import sys
from collections import Counter

SHIFT = datetime.timedelta(hours=8)
# 通用计数键：迁移只改桶键，这些合计必须逐项相等。
USAGE_SUM_KEYS = ("q", "e", "p", "c", "t")


def shift_key(scope):
    """h:YYYY-MM-DDTHH -> +8h 后的同格式键。"""
    if not scope.startswith("h:"):
        raise SystemExit("非小时桶键，拒绝迁移: %r" % scope)
    dt = datetime.datetime.strptime(scope[2:], "%Y-%m-%dT%H")
    return "h:" + (dt + SHIFT).strftime("%Y-%m-%dT%H")


def check_scopes(path, buckets):
    """确认全部是小时桶；出现日桶就停下（需要重新折叠，不是平移）。"""
    scopes = Counter(b["s"][:1] for b in buckets)
    if scopes.get("d"):
        raise SystemExit(
            "%s: 含 %d 个日桶（d:）。日桶横跨两个时区日，平移会把数据算错——"
            "需要用小时数据在新时区下重新折叠后再迁移。" % (path, scopes["d"])
        )
    if scopes.get("h") != len(buckets):
        raise SystemExit("%s: 存在未知桶键前缀: %r" % (path, dict(scopes)))
    return scopes


def write_json(path, data, apply):
    out = path + ".new"
    with open(out, "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, separators=(",", ":"))
    if apply:
        os.replace(out, path)  # 原子替换：不会留下半个文件
        print("  已替换 -> %s" % path)
    else:
        print("  演练产物 -> %s（未改原文件）" % out)


def migrate_channel(path, apply):
    with open(path, encoding="utf-8") as f:
        data = json.load(f)
    buckets = data.get("buckets", [])
    scopes = check_scopes(path, buckets)
    before = sum(sum(b["k"].values()) for b in buckets)
    for b in buckets:
        b["s"] = shift_key(b["s"])
    after = sum(sum(b["k"].values()) for b in buckets)
    if before != after:
        raise SystemExit("事件总数变化：%d -> %d" % (before, after))
    print("channel-stats: 桶=%d 类型=%s 事件=%d" % (len(buckets), dict(scopes), before))
    if buckets:
        print("  首个=%s 末个=%s" % (buckets[0]["s"], buckets[-1]["s"]))
    write_json(path, data, apply)


def migrate_usage(path, apply):
    with open(path, encoding="utf-8") as f:
        data = json.load(f)
    buckets = data.get("buckets", [])
    scopes = check_scopes(path, buckets)
    before = {k: sum(b.get(k, 0) for b in buckets) for k in USAGE_SUM_KEYS}
    for b in buckets:
        b["s"] = shift_key(b["s"])
    after = {k: sum(b.get(k, 0) for b in buckets) for k in USAGE_SUM_KEYS}
    if before != after:
        raise SystemExit("用量合计变化：%r -> %r" % (before, after))
    print("usage: 桶=%d 类型=%s 合计=%s" % (len(buckets), dict(scopes), before))
    if buckets:
        print("  首个=%s 末个=%s" % (buckets[0]["s"], buckets[-1]["s"]))
    write_json(path, data, apply)


def main():
    ap = argparse.ArgumentParser(description="把账本小时桶键从 UTC 平移到 UTC+8")
    ap.add_argument("data_dir", help="包含 usage.json 与 channel-stats.json 的目录")
    ap.add_argument("--apply", action="store_true", help="真正替换原文件（默认只演练）")
    args = ap.parse_args()
    migrate_channel(os.path.join(args.data_dir, "channel-stats.json"), args.apply)
    migrate_usage(os.path.join(args.data_dir, "usage.json"), args.apply)
    print("完成。" if args.apply else "演练完成，加 --apply 才会改文件。")


if __name__ == "__main__":
    main()
