#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$HOME/.config/claude-cage"
exec 9>"$HOME/.config/claude-cage/antigravity-desktop-install.lock"
flock -n 9 || { echo 'Antigravity Desktop installation is already running.' >&2; exit 1; }
arch="$(dpkg --print-architecture)"
case "$arch" in
  amd64)
    deb_arch="linux-x64"
    expected_sha256="c7c9350928d3a5674a4508a74391f237a2a95af75658ad406eb46f851019610b"
    ;;
  arm64)
    deb_arch="linux-arm"
    expected_sha256="09e1551f473ee5dc4dc6fda9f2b6a9e6deeb50359a67b60ad716e17d26d0d95b"
    ;;
  *)
    echo 'Unsupported desktop architecture.' >&2
    exit 1
    ;;
esac
apt_get() {
  local status
  if sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get -o DPkg::Lock::Timeout=60 "$@" </dev/null; then
    return 0
  else
    status=$?
  fi
  printf 'APT_FAILED exit=%s command=apt-get %s\n' "$status" "$*" >&2
  return "$status"
}
printf '正在准备安装依赖…\n'
apt_get update
apt_get install -y --no-install-recommends wmctrl ca-certificates curl gnupg dbus-x11 xauth x11-utils x11-xserver-utils tigervnc-standalone-server tigervnc-tools openbox gnome-keyring gcr bubblewrap socat openssh-client libglib2.0-bin ibus ibus-gtk3 gir1.2-ibus-1.0 gir1.2-gtk-3.0 python3-gi python3-pil python3-xlib xsettingsd xdg-utils
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
printf '正在验证并下载 Google Antigravity 官方安装包…\n'
deb_url="https://storage.googleapis.com/antigravity-public/antigravity-hub/2.35.0-6394266482769920/${deb_arch}/Antigravity.deb"
cage_curl_download "$temporary/Antigravity.deb" --proto '=https' --proto-redir '=https' -fSL --connect-timeout 15 "$deb_url" -o "$temporary/Antigravity.deb"
printf '%s  %s\n' "$expected_sha256" "$temporary/Antigravity.deb" | sha256sum --check --status || { echo 'Antigravity package checksum did not match; installation stopped.' >&2; exit 1; }
printf '正在安装 Antigravity Desktop…\n'
apt_get install -y --no-install-recommends "$temporary/Antigravity.deb"
test "$(dpkg-query -W -f='${Status}' antigravity)" = 'install ok installed'
xdg-mime default workbench-web.desktop x-scheme-handler/http x-scheme-handler/https 2>/dev/null || true
printf 'Antigravity Desktop 已安装。\n'
