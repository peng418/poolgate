#!/bin/bash
# ============================================================================
# build-fpk.sh —— 把 poolgate 打成飞牛 fnOS 原生 FPK（端口 5014 版）
# 移植自 wild-work 的实测打包脚本（同一 fnOS 框架约定），改动：
#   appname=poolgate、端口 5014、载荷 poolgate 单二进制、独立用户 poolgate
#
# 用法: ./build-fpk.sh [版本号] [输出目录]
# ============================================================================
set -euo pipefail

VERSION="${1:-0.1.2}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${2:-$HERE/dist}"   # 第二个参数可指向任意目录（如本机的交付目录）
TPL="$HERE/tpl"
PAYLOAD="$HERE/payload/poolgate"
BRIDGE="$TPL/wwbridge"
ICON_SRC="$HERE/assets/appicon.png"
PKG="$HERE/.build/pkg"
DIST="$HERE/dist"
APPNAME=poolgate
PORT=5014
FNPAK="$HOME/.local/bin/fnpack"

say() { printf '\n\033[1;36m=== %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mFATAL: %s\033[0m\n' "$*" >&2; exit 1; }

say "0. 前置检查"
[ -x "$PAYLOAD" ] || die "缺主程序载荷: $PAYLOAD（先在 src/ 跑 build.sh 并拷贝过来）"
[ -x "$BRIDGE" ]  || die "缺网关桥: $BRIDGE"
[ -f "$ICON_SRC" ]|| die "缺图标源: $ICON_SRC"
[ -x "$FNPAK" ]   || die "缺 fnpack: $FNPAK"

say "1. 组装 FPK 目录 ($PKG)"
rm -rf "$PKG"
mkdir -p "$PKG/app/bin" "$PKG/app/ui/images" "$PKG/cmd" "$PKG/config" "$PKG/wizard"

install -m 0755 "$PAYLOAD" "$PKG/app/bin/poolgate"
install -m 0755 "$BRIDGE"  "$PKG/app/bin/wwbridge"
install -m 0644 "$TPL/bin-config.json" "$PKG/app/bin/config.json"

install -m 0644 "$TPL/ui/config"   "$PKG/app/ui/config"
install -m 0755 "$TPL/ui/index.cgi" "$PKG/app/ui/index.cgi"

for f in main install_init install_callback upgrade_init upgrade_callback \
         uninstall_init uninstall_callback config_init config_callback; do
    install -m 0755 "$TPL/cmd/$f" "$PKG/cmd/$f"
done

install -m 0644 "$TPL/privilege" "$PKG/config/privilege"
install -m 0644 "$TPL/resource"  "$PKG/config/resource"

for w in install upgrade config; do install -m 0644 "$TPL/wizard/$w" "$PKG/wizard/$w"; done

say "2. 生成图标（64/256）"
python3 - "$ICON_SRC" "$PKG" <<'PY'
import sys
from PIL import Image
src, pkg = sys.argv[1], sys.argv[2]
im = Image.open(src).convert("RGBA")
im.resize((64, 64),  Image.LANCZOS).save(f"{pkg}/app/ui/images/icon_64.png")
im.resize((256, 256), Image.LANCZOS).save(f"{pkg}/app/ui/images/icon_256.png")
print("  64x64 / 256x256 已生成")
PY
cp "$PKG/app/ui/images/icon_64.png"  "$PKG/ICON.PNG"
cp "$PKG/app/ui/images/icon_256.png" "$PKG/ICON_256.PNG"
# 与原版包保持同样的权限形态（原版这三个文件都是 0644）
chmod 0644 "$PKG/ICON.PNG" "$PKG/ICON_256.PNG" "$PKG/app/ui/images/icon_64.png" "$PKG/app/ui/images/icon_256.png"

say "3. 生成 manifest"
M="$PKG/manifest"
{
  printf 'appname               = %s\n'   "$APPNAME"
  printf 'version               = %s\n'   "$VERSION"
  printf 'display_name          = PoolGate\n'
  printf 'desc                  = PoolGate AI 账号池网关。把 20+ 家 AI 登录态（QoderCN / TraeWork / WorkBuddy / 豆包 / Kimi / 智谱 / ChatGPT / Claude 订阅 / Copilot / Kiro / iFlow / 灵码 / Antigravity / Windsurf / 通义 / Gemini 等）聚合为 OpenAI/Anthropic/Codex 兼容 API（/v1）与内置 Web 控制台，端口 5014，IPv4+IPv6 双栈；与 wild-work 并存独立运行。\n'
  printf 'arch                  = x86_64\n'
  printf 'platform              = x86\n'
  printf 'source                = thirdparty\n'
  printf 'maintainer            = poolgate\n'
  printf 'distributor           = poolgate\n'
  printf 'ctl_stop              = true\n'
  printf 'service_port          = %s\n'   "$PORT"
  printf 'desktop_uidir         = ui\n'
  printf 'desktop_applaunchname = %s.main\n' "$APPNAME"
  printf 'changelog = 千问办公不再因为带 tools 被顶死（真机 2026-09-26）：千问办公（qwenwork）的网页协议是 chat-ws 的纯文本 new_prompt，确实没有放工具定义的位置；但客户端（本项目自己的 coding agent）习惯性挂 tools 时，整轮请求会被网关明确拒绝（HTTP 400），于是表现为「千问办公用不了」。新增 channel.Spec.ToolsIgnore 能力档：声明它的渠道（现为千问办公）收到带 tools 的请求时不报错，网关把这批 tools 丢掉、按纯文本转发，并落一条日志（面板日志可见，不静默）；工具位仍如实标 CapNo —— 客户端拿不到 tool_calls，这一点不撒谎。默认档不变：既不原生、也不代做模拟、更没声明忽略的渠道，带 tools 仍然明确拒绝（红线一）。OpenAI 与 Anthropic 两个入口走同一条合同。新增 4 条回归测试：忽略档放行且不把工具说明塞进提示词、默认档仍 400 且不打上游、Anthropic 入口同样放行、只有千问办公声明忽略（其它渠道不许悄悄变成忽略）。 | FPK %s\n' "$VERSION"
} >> "$M"
chmod 0644 "$M"
sed -n '1,20p' "$M" | sed 's/^/  /'

say "4. 打包前自检"
LINKS=$(find "$PKG" -type l | wc -l)
[ "$LINKS" -eq 0 ] || die "包内含 $LINKS 个 symlink（会触发 acl_get_file failed / code 10234）"
echo "  symlink 数量: 0 ✓"
echo "  cmd/ 权限:"; find "$PKG/cmd" -type f -not -perm -u+x -printf '    !! 不可执行: %p\n' | sed -n '1,5p'; echo "    全部可执行 ✓"
echo "  JSON 合法性:"
for f in "$PKG/app/ui/config" "$PKG/config/privilege" "$PKG/config/resource" "$PKG/wizard/install" "$PKG/wizard/upgrade" "$PKG/wizard/config" "$PKG/app/bin/config.json"; do
  python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$f" && echo "    ok $(basename $(dirname $f))/$(basename $f)"
done

# 向导内容必须非空且每步都有标题：飞牛应用中心解析向导时，空数组会让它
# 直接报「应用包不符合系统要求」（实测：Code 10111 + nil pointer dereference）。
# 这条检查是那次踩坑的直接产物，别删。
echo "  安装向导内容:"
python3 - "$PKG" <<'PY'
import json, sys, os
pkg = sys.argv[1]
bad = []
for name in ("install", "upgrade", "config"):
    path = os.path.join(pkg, "wizard", name)
    try:
        data = json.load(open(path, encoding="utf-8"))
    except Exception as e:
        bad.append(f"{name}: 不是合法 JSON（{e}）")
        continue
    if not isinstance(data, list) or not data:
        bad.append(f"{name}: 必须是非空 JSON 数组（飞牛要求至少一步）")
        continue
    for i, step in enumerate(data):
        title = (step or {}).get("stepTitle", "")
        if not title.strip():
            bad.append(f"{name}[{i}]: stepTitle 为空（应用中心会拒绝安装）")
        items = (step or {}).get("items")
        if not isinstance(items, list) or not items:
            bad.append(f"{name}[{i}]: items 必须是非空数组")
            continue
        for j, it in enumerate(items):
            if not (it or {}).get("type") or not (it or {}).get("helpText"):
                bad.append(f"{name}[{i}].items[{j}]: 缺 type 或 helpText")
    print(f"    ok wizard/{name}: {len(data)} 步")
if bad:
    print("    !! 向导内容不合格：")
    for b in bad:
        print("       " + b)
    sys.exit(1)
PY

say "5. fnpack build"
( cd "$PKG" && "$FNPAK" build -d . ) || die "fnpack build 失败"
RAW="$PKG/$APPNAME.fpk"
[ -f "$RAW" ] || die "未生成 $RAW"
mkdir -p "$DIST"
FPK="$DIST/$APPNAME-$VERSION.fpk"
mv -f "$RAW" "$FPK"
echo "  产物: $FPK"
echo "  大小: $(du -h "$FPK" | cut -f1) ($(stat -c%s "$FPK") B)"
echo "  SHA256: $(sha256sum "$FPK" | cut -d' ' -f1)"

say "6. 产物内容核对"
tar tzf "$FPK" | sed 's/^/  /'

say "7. 落到 $OUT_DIR"
if [ -d "$OUT_DIR" ] && [ -w "$OUT_DIR" ]; then
  cp -f "$FPK" "$OUT_DIR/" && echo "  已复制: $OUT_DIR/$(basename "$FPK")"
else
  echo "  目标目录不可写 —— FPK 保留在: $FPK"
fi
say "完成：$FPK"
