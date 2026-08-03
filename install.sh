#!/bin/sh
# local-mirror 安装脚本：识别系统与架构，下载最新 Release 的二进制，
# 校验 checksum 后安装，并在需要时把安装目录加进 PATH。
#
#   curl -fsSL https://raw.githubusercontent.com/xwvike/local-mirror/main/install.sh | sh
#
# 可用环境变量覆盖：
#   VERSION=v0.9.0     安装指定版本（默认最新）
#   WITH_SERVICE=1     安装后顺带执行 `local-mirror service install` 装成系统服务
#   INSTALL_DIR=/path  安装目录。默认按身份走：root 装 /usr/local/bin，
#                      普通用户装 ~/.local/bin——脚本自身从不提权，
#                      运行时同样以调用者权限做文件操作
set -eu

REPO="xwvike/local-mirror"

err() { printf 'install.sh: %s\n' "$1" >&2; exit 1; }

os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) err "unsupported OS: $os (on Windows use: scoop install local-mirror)" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) err "unsupported architecture: $arch" ;;
esac

ver=${VERSION:-}
if [ -z "$ver" ]; then
	ver=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
	[ -n "$ver" ] || err "failed to resolve the latest version"
fi
ver=${ver#v}

name="local-mirror_${ver}_${os}_${arch}"
base="https://github.com/$REPO/releases/download/v${ver}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading local-mirror v${ver} (${os}/${arch})..."
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

want=$(grep " ${name}.tar.gz\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$want" ] || err "$name.tar.gz not found in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)
else
	got=$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)
fi
[ "$got" = "$want" ] || err "checksum mismatch (download may be truncated or tampered with)"

tar xzf "$tmp/$name.tar.gz" -C "$tmp" local-mirror

if [ -n "${INSTALL_DIR:-}" ]; then
	dir=$INSTALL_DIR
elif [ "$(id -u)" -eq 0 ]; then
	dir=/usr/local/bin
else
	dir="$HOME/.local/bin"
fi
mkdir -p "$dir"
# 不用 install(1)：busybox 没有它（OpenWrt 等）。
# 先 rm 再 cp——覆盖正在运行的可执行文件在 Linux 上会 ETXTBSY「文本文件忙」，
# 在 Apple Silicon 上则会让新副本因签名失效被内核 SIGKILL；换个 inode 两者都绕开
rm -f "$dir/local-mirror"
cp "$tmp/local-mirror" "$dir/local-mirror"
chmod 755 "$dir/local-mirror"

case ":$PATH:" in
*":$dir:"*) ;;
*)
	rc=""
	case "${SHELL:-}" in
	*/zsh) rc="$HOME/.zshrc" ;;
	*/bash) rc="$HOME/.bashrc" ;;
	esac
	if [ -n "$rc" ]; then
		if ! grep -qs '# added by local-mirror install.sh' "$rc"; then
			printf '\n# added by local-mirror install.sh\nexport PATH="%s:$PATH"\n' "$dir" >>"$rc"
		fi
		echo "added $dir to PATH (written to ${rc}; open a new shell to apply)"
	else
		echo "note: $dir is not on your PATH; add it in your shell config"
	fi
	;;
esac

echo "installed: $dir/local-mirror"
"$dir/local-mirror" --version

# 服务安装委托给二进制自己：它按平台生成 systemd unit / launchd plist /
# procd init 脚本，逻辑有单测覆盖，不在这里用 shell 重写一遍
if [ "${WITH_SERVICE:-0}" = 1 ]; then
	echo
	"$dir/local-mirror" service install
else
	echo
	echo "install it as a service with:  $dir/local-mirror service install"
	echo "(or re-run this script with WITH_SERVICE=1)"
fi
