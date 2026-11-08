#!/usr/bin/env bash
# login.sh — WorkBuddy 授权向导封装（国内版/国际版）。
#
# 用法:
#   ./login.sh            # 国内版（codebuddy.cn），nickname 默认等于键名
#   ./login.sh intl       # 国际版（codebuddy.ai）→ 键前缀 codebuddyai
#   ./login.sh cn myname  # 指定显示名
#
# 其余供应商走统一向导（login <平台> <名称>，在容器内或本机直接）：
#   ./login trae 我的trae
#   ./login qoder 我的qoder
#
# 流程（由 cmd/login 内部完成）：打印「如何配置」→ 粘贴凭证 JSON → 写入 auth.json →
# 打印「✅ 配置成功」→ 重启网关加载新账号。
# 签到 → 写入 auths/auth.json（键如 codebuddy_1 / codebuddyai_1）→ 重启网关。
set -euo pipefail

REGION="${1:-cn}"
NICK="${2:-}"

cd "$(dirname "$0")"

# login 工具：不存在才编译
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

exec "$LOGIN_BIN" codebuddy "$REGION" "$NICK"
