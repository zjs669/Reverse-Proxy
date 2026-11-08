#!/usr/bin/env python3
# 为 auths/auth.json 里每个账号生成一把专属网关 key，写入 config.json 的 account_keys（嵌套结构）。
# account_keys 形如 {名字: {uid: 密钥}}；一把 key 锁一个账号：dsh 里一个号一个提供方条目，请求只走该号（不轮换）。
#
# 同时刷新《账号令牌清单.md》（账号专属密钥速查表）。
#
# 用法：
#   python3 gen_account_keys.py            # 为所有账号生成/补齐 key（保留已有）
#   python3 gen_account_keys.py --rotate   # 全部重置（换新 key）
#   python3 gen_account_keys.py --dry      # 只打印计划，不写文件
#
# auth.json 顶层是一张 map（键=账号标签，如 qoder_1 / trae_1 / codebuddy_2），条目兼容：
#   - codebuddy 嵌套形 {"account":{...},"auth":{...}}
#   - trae / qoder / doubao 扁平形（uid/nickname/provider 在顶层）
# provider 优先级：条目内 provider 字段 > 标签前缀推断（codebuddy/codebuddyai→codebuddy）。
import json, os, secrets, sys, shutil, datetime

ROOT = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(ROOT, "config.json")
DOC = os.path.join(ROOT, "账号令牌清单.md")

rotate = "--rotate" in sys.argv
dry = "--dry" in sys.argv

# 标签前缀 → provider（长前缀优先匹配，codebuddyai 要在 codebuddy 之前命中）。
PREFIX_PROVIDER = {
    "codebuddyai": "codebuddy",
    "workbuddy": "codebuddy",
    "codebuddy": "codebuddy",
    "trae": "trae",
    "qoder": "qoder",
    "doubao": "doubao",
}


def auth_file_path(cfg):
    af = (cfg.get("auth_file") or "").strip() or "./auths/auth.json"
    if not os.path.isabs(af):
        af = os.path.join(ROOT, af)
    return af


def infer_provider(name, entry):
    p = (entry.get("provider") or "").strip().lower()
    if p:
        return p
    p = (entry.get("auth", {}) or {}).get("provider", "").strip().lower()
    if p:
        return p
    base = (name or "").lower()
    for pref, prov in sorted(PREFIX_PROVIDER.items(), key=lambda kv: -len(kv[0])):
        if base.startswith(pref):
            return prov
    return "codebuddy"


def load_accounts(cfg):
    fp = auth_file_path(cfg)
    if not os.path.exists(fp):
        sys.exit("找不到凭证文件: %s" % fp)
    try:
        data = json.load(open(fp, encoding="utf-8"))
    except Exception as e:
        sys.exit("读取 auth.json 失败: %s" % e)
    if not isinstance(data, dict):
        sys.exit("auth.json 顶层应为 {名字: 条目} map")
    out = []
    for name, entry in data.items():
        if not isinstance(entry, dict):
            continue
        acct = entry.get("account", {}) or {}
        auth = entry.get("auth", {}) or {}
        uid = acct.get("uid") or entry.get("uid") or auth.get("uid") or ""
        if not uid:
            continue
        out.append({
            "name": name,
            "uid": uid,
            "nickname": acct.get("nickname") or entry.get("nickname") or "",
            "provider": infer_provider(name, entry),
            "domain": auth.get("domain", "") or entry.get("domain", ""),
            "expires_at": auth.get("expiresAt") or entry.get("expiresAt") or 0,
        })
    return out


def fmt_ts(v):
    try:
        return datetime.datetime.fromtimestamp(int(v)).strftime("%Y-%m-%d %H:%M:%S")
    except Exception:
        return "—"


def gen_key():
    return secrets.token_hex(32)


cfg = json.load(open(CFG, encoding="utf-8"))
accounts = load_accounts(cfg)
if not accounts:
    sys.exit("auth.json 里没有可用账号")

# 保留既有 key（除非 --rotate），按「名字 → {uid: secret}」复用；缺失才新生成。
# 旧扁平结构（uid→secret 顶层）读不到名字对应值 → 自动重生成，即自迁移为嵌套。
existing = dict(cfg.get("account_keys", {}))
new_map = {}
plan = []
for a in accounts:
    name, uid = a["name"], a["uid"]
    k = None
    if not rotate:
        prev = existing.get(name)
        if isinstance(prev, dict):
            k = prev.get(uid)
    if not k:
        k = gen_key()
    new_map[name] = {uid: k}
    a["key"] = k
    plan.append(a)

if dry:
    print("[dry-run] 将写入 account_keys：")
    for name, m in new_map.items():
        for uid, k in m.items():
            print(f"  {name}  {uid}  ->  {k}")
    sys.exit(0)

# 备份 config.json
bak = CFG + ".bak." + datetime.datetime.now().strftime("%Y%m%d%H%M%S")
shutil.copy2(CFG, bak)

cfg["account_keys"] = new_map
with open(CFG, "w", encoding="utf-8") as f:
    json.dump(cfg, f, indent=2, ensure_ascii=False)
    f.write("\n")

print("已更新 config.json 的 account_keys（共 %d 把，嵌套结构），备份：%s" % (len(new_map), os.path.basename(bak)))
for a in plan:
    print(f"  [{a['provider']}] {a['name']}  {a['nickname'] or a['uid'][:8]}  {a['uid']}  {a['key']}")

# ---- 同步刷新速查文档 ----
L = []
L.append("# Reverse-Proxy 账号凭证速查（精简版）")
L.append("")
L.append("> ⚠️ 含网关密钥，勿外传 / 勿提交 git。")
L.append("")
L.append(f"账号数：**{len(accounts)}**　|　每号一把专属 key（`account_keys`，锁死单号，不轮换）")
L.append("")
L.append("| 账号标签 | 供应商 | 用户名 | 过期时间 | uid | 该账号专属 key（dsh API 密钥） |")
L.append("| --- | --- | --- | --- | --- | --- |")
for a in accounts:
    L.append("| `%s` | `%s` | %s | %s | `%s` | `%s` |" % (
        a["name"], a["provider"], a["nickname"] or "—", fmt_ts(a["expires_at"]), a["uid"], a["key"]))
L.append("")
L.append("> 鉴权只认 `account_keys`（已取消主密钥 / 区域密钥 / 供应商密钥）。")
L.append("> 凭证格式、协议细节、模板与排错见 **PROVIDERS.md**。")
L.append("")

with open(DOC, "w", encoding="utf-8") as f:
    f.write("\n".join(L))
print("已更新文档:", DOC)
