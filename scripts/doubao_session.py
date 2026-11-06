#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""doubao_session.py — 把浏览器里的豆包 Cookie 落成网关凭证文件。

豆包（doubao）走网页版 Cookie 鉴权：唯一必需凭证是 sessionid，配套 ttwid /
passport_csrf_token 可显著降低风控概率。本脚本不做任何网络请求，只做「解析 + 落盘」。

用法（三选一）：

  1) 直接喂整段 Cookie（最省事，从 F12 -> Network -> 任一 chat/completion 请求
     的 Request Headers 里复制 Cookie: 后面那一整串）：

       python3 scripts/doubao_session.py --cookie "sessionid=xxx; ttwid=yyy; ..." --uid doubao-1

  2) 只给 sessionid（其余留空，用网关内置默认设备指纹）：

       python3 scripts/doubao_session.py --sessionid "xxxx" --uid doubao-1

  3) 显式分项给：

       python3 scripts/doubao_session.py --sessionid xxx --ttwid yyy \\
           --csrf zzz --device-id 7xxxx --web-id 7yyyy --uid doubao-1

输出：auths/doubao-<uid>.json（provider=doubao），网关下次加载即生效。

安全：文件含登录态，勿提交 git / 勿外传。
"""

import argparse
import json
import os
import re
import sys

DEFAULT_OUT_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "auths")
# Cookie 无显式过期时间，填 2100-01-01，避免被网关误判过期而走「刷新」（豆包无刷新机制）。
FAR_FUTURE = 4102444800


def parse_cookie(raw: str) -> dict:
    """把 'a=1; b=2' 解析为 dict（容忍换行、多余空格、尾分号）。"""
    out = {}
    for part in re.split(r"[;\n\r]+", raw or ""):
        part = part.strip()
        if not part or "=" not in part:
            continue
        k, v = part.split("=", 1)
        k, v = k.strip(), v.strip().strip('"')
        if k:
            out[k] = v
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description="生成 auths/doubao-<uid>.json（豆包 Cookie 凭证）")
    ap.add_argument("--cookie", help="整段 Cookie 字符串（含 sessionid）")
    ap.add_argument("--sessionid", help="sessionid Cookie 值")
    ap.add_argument("--ttwid", help="ttwid Cookie 值（可选，建议提供）")
    ap.add_argument("--csrf", dest="csrf", help="passport_csrf_token Cookie 值（可选）")
    ap.add_argument("--device-id", dest="device_id", help="19 位 device_id（可选）")
    ap.add_argument("--web-id", dest="web_id", help="19 位 web_id（可选）")
    ap.add_argument("--fp", help="指纹令牌 verify_xxx（可选）")
    ap.add_argument("--uid", default="doubao-1", help="账号标识，写入 uid 并用于文件名（默认 doubao-1）")
    ap.add_argument("--nickname", help="昵称（默认同 uid）")
    ap.add_argument("--out-dir", default=DEFAULT_OUT_DIR, help="输出目录（默认 <repo>/auths）")
    ap.add_argument("--force", action="store_true", help="目标文件已存在时覆盖")
    args = ap.parse_args()

    cookies = parse_cookie(args.cookie) if args.cookie else {}
    sessionid = (args.sessionid or cookies.get("sessionid") or "").strip()
    if not sessionid:
        print("ERROR: 缺少 sessionid（--sessionid 或 --cookie 里需含 sessionid=...）", file=sys.stderr)
        return 2

    ttwid = (args.ttwid or cookies.get("ttwid") or "").strip()
    csrf = (args.csrf or cookies.get("passport_csrf_token") or "").strip()
    device_id = (args.device_id or cookies.get("device_id") or "").strip()
    web_id = (args.web_id or cookies.get("web_id") or "").strip()
    fp = (args.fp or cookies.get("fp") or "").strip()

    uid = re.sub(r"[^0-9A-Za-z._-]", "-", args.uid.strip() or "doubao-1")
    extra = {}
    if ttwid:
        extra["ttwid"] = ttwid
    if csrf:
        extra["passport_csrf_token"] = csrf
    if device_id:
        extra["device_id"] = device_id
    if web_id:
        extra["web_id"] = web_id
    if fp:
        extra["fp"] = fp

    doc = {
        "provider": "doubao",
        "accessToken": sessionid,
        "refreshToken": "",
        "expiresAt": FAR_FUTURE,
        "uid": uid,
        "nickname": args.nickname or uid,
        "extra": extra,
    }

    os.makedirs(args.out_dir, exist_ok=True)
    path = os.path.join(args.out_dir, "doubao-%s.json" % uid)
    if os.path.exists(path) and not args.force:
        print("ERROR: %s 已存在（加 --force 覆盖）" % path, file=sys.stderr)
        return 3
    with open(path, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False, indent=2)
        f.write("\n")

    # 只回显长度，不打印明文 token
    print("OK  写入 %s" % path)
    print("    provider=doubao  uid=%s  sessionid(len)=%d  ttwid=%s  csrf=%s"
          % (uid, len(sessionid), "有" if ttwid else "无", "有" if csrf else "无"))
    print("    下一步：确认 config.json 的 provider_keys 已配 doubao 密钥，然后 docker compose up -d（或重启容器）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
