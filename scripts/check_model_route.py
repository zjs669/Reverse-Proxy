#!/usr/bin/env python3
"""核对「客户端模型名 → 上游真实模型 key」的映射是否正确。

用途：当某个模型名在网关里表现和预期不符（比如以为切到了 Pro，实际还是默认档），
用它发一次极简请求，再从网关日志里捞出本次请求实际用的 upstreamKey。

用法：
    export WB2API_KEY=<你在 config.json 里配的 account_keys 密钥>
    python scripts/check_model_route.py "deepseek-v4.1-flash:high"

依赖：pip install openai；网关以容器名 workbuddy2api 运行（否则改 CONTAINER）。
"""
import os
import re
import subprocess
import sys

from openai import OpenAI

BASE_URL = os.environ.get("WB2API_BASE", "http://127.0.0.1:7863/v1")
API_KEY = os.environ.get("WB2API_KEY", "")
CONTAINER = os.environ.get("WB2API_CONTAINER", "workbuddy2api")
MODEL = sys.argv[1] if len(sys.argv) > 1 else "DeepSeek-V4.1-Flash:high"

if not API_KEY:
    sys.exit("请先设置 WB2API_KEY 环境变量（config.json 里 account_keys 的某个密钥）")

client = OpenAI(base_url=BASE_URL, api_key=API_KEY)

# 发一次极简请求，只为让网关走一遍模型路由（响应内容不看）
try:
    client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "."}],
        max_tokens=1,
    )
except Exception as e:  # noqa: BLE001
    print(f"[warn] 请求异常（不影响读路由日志）: {e}")

# 直接看实际调用的模型：读网关日志里本次的 client -> upstreamKey
logs = subprocess.run(
    ["docker", "logs", "--tail", "200", CONTAINER],
    capture_output=True, text=True, encoding="utf-8", errors="replace",
)
combined = (logs.stdout or "") + "\n" + (logs.stderr or "")
hit = None
for line in combined.splitlines():
    if "chat model:" in line and f'client="{MODEL}"' in line:
        hit = line  # 取最后一条 = 本次请求

m = re.search(r'upstreamKey="([^"]*)"', hit) if hit else None
real_key = m.group(1) if m else "?"

print("=" * 48)
print(f"你请求的模型名 : {MODEL}")
print(f"实际调用的 key : {real_key}")
print(f"是否被翻译     : {'否(名字即key)' if real_key == MODEL else '是(名字≠真实key)'}")
print("=" * 48)
