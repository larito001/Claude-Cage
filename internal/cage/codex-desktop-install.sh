#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$HOME/.config/claude-cage"
exec 9>"$HOME/.config/claude-cage/codex-desktop-install.lock"
flock -n 9 || { echo 'Codex Desktop installation is already running.' >&2; exit 1; }
arch="$(dpkg --print-architecture)"
case "$arch" in arm64|amd64) ;; *) echo 'Unsupported desktop architecture.' >&2; exit 1;; esac
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
apt_get install -y --no-install-recommends wmctrl ca-certificates curl gnupg dbus-x11 xauth x11-utils x11-xserver-utils tigervnc-standalone-server tigervnc-tools openbox gnome-keyring gcr bubblewrap socat openssh-client libglib2.0-bin ibus ibus-gtk3 gir1.2-ibus-1.0 gir1.2-gtk-3.0 python3-gi python3-pil python3-xlib xsettingsd xdg-utils xz-utils
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
printf '正在验证 OpenAI 安装源…\n'
signing_key_base64='mQINBGpypFUBEACi1Vvzq9pIpA6lj7chbqELuxJtVuzUzxrasa6ZU0yF4yhq7jf83YkJRHwbezBKeQyzJ5lkX0EhXS8aXxUhMAm3PFpAlwcInfKzmV7atJwvaxIw6RmdGYe9fBWKjTN/SmPIjtyxrTznZY97+TfD1AeGZpLaJ8fsnhrC+HkiN2TACiTocgpehFiP0OWK7mWZeTWnY2scpIYXP1Ro7nQv4KacmY4JacTQ7m/HM0Qej/3olhuEv2CwlMVWw57/oHhmTllfLDQOogFQyIVqaaR98y/Eu6cAabSfcsqAAZ2A8vfHYD27z28JvLO2PZEJd5ThlnX4Zqv0eIpZdBj//8Sl/MSqTshFZ1NDsRoqwdqw284X5MpnOJ4k4Sc2Se8tJxt/nCeibH3dJ504Fb1X/mnOqhCAQ6pVJz4RB5HRlFPSkxVPyag1v1m/7T4vie+OR4eqFQNz6mudrOoMmeVIfyL5fbe4cOr4fk/FyvEE2xMgkFatPqXn7vM9og+zremPCfwRAFpBPyX74VowFY7llcdaj/w8K5T8PzM14Hb3E4ZKizMluKmTvTq9WE1/eSQJLLQqXD5VmtmdUaC/VyE/1ZlIxcA1LWqvEQ327UXREvX/nHsrkKrl956WjzkiHFUTsD1NJ0dMfs+csOt8Furb5jZj+HsMmCm9jLdfz5b/4WKLPbvxIwARAQABtBZDb2RleCBMaW51eCBSZXBvc2l0b3J5iQJRBBMBCgA7FiEEO/oOSui4zBai2bpoSjtKVmxGYOQFAmpypFUCGwMFCwkIBwICIgIGFQoJCAsCBBYCAwECHgcCF4AACgkQSjtKVmxGYORlCQ/9FyikZo8HQcJBP9E/oXVPds/fQnIFB2qJR2z3DrfYEonNt/evSAySkPPq4/mEOjaI0pFlDDGSaps+FTcJFgoVRTasBIF7JJivvjW9ap8iWEbhhVLeIrFLbMLpUcTRntUx7R4fVMJ/1/cGn+NWZmNwS9ORorzSyCH0IAgCw1Xc3ZrjuMbFVjdToMC1TiXXCEmlYpQakmQ3Ay1cH0FHC2BBNn1MNVkJdPhpZIZCdhaMPHfYFpyopg8wFvZ5iIcvlbMgyuy8CPJVRWUcYy2dOhEOGnYJnXRPkE3E1hf8YOHNzRlduH896lT9qcEK2+fpLfrVGoc4zscLZ+Ey+Ko6iQRdVE1j67+wNR3hX8ukue574v1N/xxui575jumSE19lEj1sH4+P4gFHOtTbF0JhKKzLctbga0IAwTPKhnt3qz31U5Yj/MZSuEVjrLhdRauOuFBXUclgyVf2w/lE85UUOdlcollsYA6Huq7xDamqf8SslZQGre3EI+lhpqJR1cOwDMUzzcl40uTyhrxXXd/bk4QSlhZbwHR25Pnt+ZMtWavlQWS0eDEV8djuXAURCmx5WOqAFB/TJe1mn5EvyWg4VFzrY/NVNOpzgY5+Xp7J28z7f637r712Eu9j4imVcdPigwS+jf/0f81i2o9b82Y26TN8+EtDLCY841MJ1lrjDrX/dno='
printf '%s' "$signing_key_base64" | base64 -d > "$temporary/key.gpg"
fingerprint=$(gpg --batch --show-keys --with-colons "$temporary/key.gpg" | awk -F: '$1 == "fpr" {print $10; exit}')
test "$fingerprint" = 3BFA0E4AE8B8CC16A2D9BA684A3B4A566C4660E4 || { echo 'OpenAI signing key did not match; installation stopped.' >&2; exit 1; }
sudo install -m 0644 "$temporary/key.gpg" /usr/share/keyrings/chatgpt-archive-keyring.gpg
sudo rm -f /etc/apt/sources.list.d/chatgpt.sources
printf 'deb [arch=%s signed-by=/usr/share/keyrings/chatgpt-archive-keyring.gpg] https://persistent.oaistatic.com/codex-app-prod/linux/deb stable main\n' "$arch" | sudo tee /etc/apt/sources.list.d/chatgpt.list >/dev/null
apt_get update
if ! apt-cache show chatgpt >/dev/null 2>&1; then
  echo "当前客机架构 ($arch) 在 OpenAI 官方 APT 源中尚无可用的 chatgpt 桌面安装包；Codex CLI 仍可正常使用。" >&2
  exit 1
fi
printf '正在下载并安装 Codex Desktop…\n'
apt_get install -y --no-install-recommends chatgpt
test "$(dpkg-query -W -f='${Status}' chatgpt)" = 'install ok installed'
xdg-mime default workbench-web.desktop x-scheme-handler/http x-scheme-handler/https 2>/dev/null || true
printf 'Codex Desktop 已安装。\n'
