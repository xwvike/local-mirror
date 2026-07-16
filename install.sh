#!/bin/sh
# local-mirror 安装脚本：识别系统与架构，下载最新 Release 的二进制，
# 校验 checksum 后安装，并在需要时把安装目录加进 PATH。
#
#   curl -fsSL https://raw.githubusercontent.com/xwvike/local-mirror/main/install.sh | sh
#
# 可用环境变量覆盖：
#   VERSION=v0.9.0     安装指定版本（默认最新）
#   INSTALL_DIR=/path  安装目录（默认 ~/.local/bin——普通用户即可安装使用，
#                      全程不需要 root/sudo；要装系统目录请自行指定并授权）
set -eu

REPO="xwvike/local-mirror"

err() { printf 'install.sh: %s\n' "$1" >&2; exit 1; }

os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) err "不支持的系统: $os（Windows 请用 scoop install local-mirror）" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) err "不支持的架构: $arch" ;;
esac

ver=${VERSION:-}
if [ -z "$ver" ]; then
	ver=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
	[ -n "$ver" ] || err "无法获取最新版本号"
fi
ver=${ver#v}

name="local-mirror_${ver}_${os}_${arch}"
base="https://github.com/$REPO/releases/download/v${ver}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "下载 local-mirror v${ver}（${os}/${arch}）..."
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

want=$(grep " ${name}.tar.gz\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$want" ] || err "checksums.txt 里找不到 $name.tar.gz"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)
else
	got=$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)
fi
[ "$got" = "$want" ] || err "checksum 校验失败（下载可能被截断或篡改）"

tar xzf "$tmp/$name.tar.gz" -C "$tmp" local-mirror

dir=${INSTALL_DIR:-"$HOME/.local/bin"}
mkdir -p "$dir"
install -m 755 "$tmp/local-mirror" "$dir/local-mirror"

case ":$PATH:" in
*":$dir:"*) ;;
*)
	rc=""
	case "${SHELL:-}" in
	*/zsh) rc="$HOME/.zshrc" ;;
	*/bash) rc="$HOME/.bashrc" ;;
	esac
	if [ -n "$rc" ]; then
		if ! grep -qs '# local-mirror install.sh 添加' "$rc"; then
			printf '\n# local-mirror install.sh 添加\nexport PATH="%s:$PATH"\n' "$dir" >>"$rc"
		fi
		echo "已把 $dir 加进 PATH（写入 ${rc}，开新终端生效）"
	else
		echo "注意: $dir 不在 PATH 里，请自行加进 shell 配置"
	fi
	;;
esac

echo "安装完成: $dir/local-mirror"
"$dir/local-mirror" --version
