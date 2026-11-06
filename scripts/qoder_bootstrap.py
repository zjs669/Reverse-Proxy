#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Qoder 凭证引导脚本（配合 Reverse-Proxy 网关的 provider="qoder"，国内版）。

网关代码（internal/upstream/qoder.go）要求 qoder*.json 里：
  - provider     = "qoder"
  - accessToken  = jt-（jobToken，等价 dt-，作 COSY 的 Bearer）
  - refreshToken = drt- 或 PAT 兑换返回的 refresh_token（可空，空时靠 extra.pat 续期）
  - expiresAt    = jt- 过期时间（Unix 秒）
  - extra.machineID / extra.machineToken / extra.machineType = COSY 持久机器指纹
  - extra.pat    = PAT(pt-)，网关据此在临期时自动换新 jt-（推荐，PAT 长期有效）

两条路线：
  A) 推荐 —— 只给 PAT，脚本自动换 jt-：
       python3 qoder_bootstrap.py --pat pt-xxxx --nickname qoder-1
     PAT 获取：https://qoder.com.cn/account/integrations → 创建 Personal Access Token

  B) 已有 jt-/dt- 与 drt-（例如从设备流抓到）：
       python3 qoder_bootstrap.py --dt jt-xxxx --drt drt-xxxx --uid your-uid

兑换协议（与官方 CLI 对齐）：
  POST https://openapi.qoder.com.cn/api/v1/jobToken/exchange
  headers: Content-Type/Accept json, Cosy-Version: 1.0.1, Cosy-ClientType: 5
  body:    {"personal_token": "pt-..."}
  resp:    {"token":"jt-...", "refresh_token":..., "expires_at"/"expires_in":...}
"""
import argparse
import base64
import json
import os
import secrets
import sys
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
AUTH_DIR = os.path.join(ROOT, "auths")

OPENAPI_BASE = os.environ.get("QODER_OPENAPI_BASE", "https://openapi.qoder.com.cn")
EXCHANGE_URL = os.environ.get("QODER_PAT_EXCHANGE_URL", OPENAPI_BASE + "/api/v1/jobToken/exchange")
USERINFO_URL = os.environ.get("QODER_USERINFO_URL", OPENAPI_BASE + "/api/v1/userinfo")

COSY_HEADERS = {
    "Content-Type": "application/json",
    "Accept": "application/json",
    "User-Agent": "reverse-proxy",
    "Cosy-Version": "1.0.1",
    "Cosy-ClientType": "5",
}


def uuid4() -> str:
    b = bytearray(secrets.token_bytes(16))
    b[6] = (b[6] & 0x0F) | 0x40
    b[8] = (b[8] & 0x3F) | 0x80
    h = b.hex()
    return f"{h[0:8]}-{h[8:12]}-{h[12:16]}-{h[16:20]}-{h[20:32]}"


def gen_machine_fingerprint():
    """与 qoderwork2api (cred.EnsureMachineFingerprint) 对齐。"""
    machine_id = uuid4()
    seed = (uuid4() + uuid4()).replace("-", "").encode("ascii")
    if len(seed) > 50:
        seed = seed[:50]
    machine_token = base64.urlsafe_b64encode(seed).decode("ascii").rstrip("=")  # RawURLEncoding
    machine_type = uuid4().replace("-", "")[:18]
    return {"machineID": machine_id, "machineToken": machine_token, "machineType": machine_type}


def _post_json(url: str, payload: dict, bearer: str = "") -> dict:
    headers = dict(COSY_HEADERS)
    if bearer:
        headers["Authorization"] = "Bearer " + bearer
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode("utf-8"), headers=headers, method="POST"
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode("utf-8") or "{}")


def _get_json(url: str, bearer: str) -> dict:
    headers = {k: v for k, v in COSY_HEADERS.items()}
    headers["Authorization"] = "Bearer " + bearer
    req = urllib.request.Request(url, headers=headers, method="GET")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode("utf-8") or "{}")


def exchange_pat(pat: str) -> dict:
    """PAT(pt-) → jobToken(jt-)。返回 {token, refresh, expires_at, expires_in}。"""
    try:
        data = _post_json(EXCHANGE_URL, {"personal_token": pat})
    except urllib.error.HTTPError as e:
        body = ""
        try:
            body = e.read().decode("utf-8")[:300]
        except Exception:
            pass
        sys.exit(f"错误：PAT 兑换失败 http {e.code} {body}\n"
                 f"  端点：{EXCHANGE_URL}\n"
                 f"  检查：PAT 是否过期/被吊销、是否为国内版（qoder.com.cn）令牌。")
    except Exception as e:
        sys.exit(f"错误：PAT 兑换请求失败：{e}\n  端点：{EXCHANGE_URL}")

    # 兼容 {token:...} 与 {data:{token:...}} 两种信封
    node = data.get("data") if isinstance(data.get("data"), dict) else data
    token = node.get("token") or node.get("job_token") or node.get("jobToken") or ""
    if not token:
        sys.exit("错误：兑换响应里没有 token 字段：" + json.dumps(data, ensure_ascii=False)[:300])
    return {
        "token": token,
        "refresh": node.get("refresh_token") or "",
        "expires_at": node.get("expires_at") or "",
        "expires_in": node.get("expires_in") or 0,
    }


def fetch_userinfo(jt: str) -> dict:
    """拿 uid / 昵称（失败不阻塞，退回随机 uid）。"""
    try:
        data = _get_json(USERINFO_URL, jt)
    except Exception:
        return {}
    node = data.get("data") if isinstance(data.get("data"), dict) else data
    return {
        "uid": str(node.get("user_id") or node.get("userId") or node.get("uid") or ""),
        "name": str(node.get("name") or node.get("nickname") or node.get("email") or ""),
    }


def parse_expiry(ex) -> int:
    from datetime import datetime, timezone

    if ex.get("expires_at"):
        try:
            s = str(ex["expires_at"]).replace("Z", "+00:00")
            return int(datetime.fromisoformat(s).timestamp())
        except Exception:
            pass
    v = ex.get("expires_in") or 0
    if v:
        import time

        return int(time.time()) + (v // 1000 if v > 1_000_000_000 else v)
    import time

    return int(time.time()) + 24 * 3600


def main():
    ap = argparse.ArgumentParser(description="Qoder 凭证引导")
    ap.add_argument("--pat", help="PAT(pt-)，自动换 jobToken（推荐）")
    ap.add_argument("--dt", help="已有 jt-/dt-（作 accessToken）")
    ap.add_argument("--drt", help="已有 drt-（device refresh token）")
    ap.add_argument("--uid", help="账号 uid（缺省取 userinfo，再缺省随机）")
    ap.add_argument("--nickname", default="", help="展示名（缺省取 userinfo）")
    ap.add_argument("--print-fp", action="store_true", help="只打印机器指纹并退出")
    ap.add_argument("--force", action="store_true", help="目标文件已存在时覆盖")
    args = ap.parse_args()

    fp = gen_machine_fingerprint()
    if args.print_fp:
        print(json.dumps(fp, indent=2, ensure_ascii=False))
        return

    info, ex = {}, {}
    dt = args.dt
    if not dt:
        if not args.pat:
            sys.exit("错误：必须提供 --pat(pt-)（推荐）或 --dt + --drt。")
        ex = exchange_pat(args.pat)
        dt = ex["token"]
        info = fetch_userinfo(dt)
        print(f"PAT 兑换成功：jobToken {dt[:12]}…（{len(dt)} 字符）")
    if not args.pat and not args.drt:
        sys.exit("错误：用 --dt 时需同时提供 --drt（drt-）。改用 --pat 可自动获取。")

    uid = args.uid or info.get("uid") or uuid4()
    nickname = args.nickname or info.get("name") or "qoder-account"

    extra = {
        "machineID": fp["machineID"],
        "machineToken": fp["machineToken"],
        "machineType": fp["machineType"],
    }
    if args.pat:
        extra["pat"] = args.pat  # 网关据此自动续期（临期换新 jt-）

    doc = {
        "provider": "qoder",
        "accessToken": dt,
        "refreshToken": args.drt or ex.get("refresh") or "",
        "expiresAt": parse_expiry(ex) if ex else 0,
        "uid": uid,
        "nickname": nickname,
        "extra": extra,
    }

    os.makedirs(AUTH_DIR, exist_ok=True)
    out = os.path.join(AUTH_DIR, f"qoder-{uid}.json")
    if os.path.exists(out) and not args.force:
        sys.exit(f"错误：{out} 已存在（加 --force 覆盖）")
    with open(out, "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=2, ensure_ascii=False)
        f.write("\n")

    print(f"已写入：{out}")
    print(f"  uid={uid}  nickname={nickname}")
    print(f"  expiresAt={doc['expiresAt']}  refreshToken={'有' if doc['refreshToken'] else '无'}  "
          f"extra.pat={'有（可自动续期）' if args.pat else '无'}")
    print("下一步：python3 gen_account_keys.py && docker restart reverse-proxy")


if __name__ == "__main__":
    main()
