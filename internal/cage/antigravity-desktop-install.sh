#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$HOME/.config/claude-cage"
exec 9>"$HOME/.config/claude-cage/antigravity-desktop-install.lock"
flock -n 9 || { echo 'Antigravity Desktop installation is already running.' >&2; exit 1; }
arch="$(dpkg --print-architecture)"
fallback_release="2.35.0-6394266482769920"
case "$arch" in
  amd64)
    deb_arch="linux-x64"
    fallback_sha256="c7c9350928d3a5674a4508a74391f237a2a95af75658ad406eb46f851019610b"
    ;;
  arm64)
    deb_arch="linux-arm"
    fallback_sha256="09e1551f473ee5dc4dc6fda9f2b6a9e6deeb50359a67b60ad716e17d26d0d95b"
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
# Clear any interrupted local .deb unpack state (reinstreq) that blocks apt-get,
# and remove duplicate .sources files when a matching .list already exists.
if dpkg-query -W -f='${Status}\n' antigravity 2>/dev/null | grep -Eq 'reinstreq|half-installed|half-configured|unpacked'; then
  printf '妫€娴嬪埌涓婃鏈畬鎴愮殑 Antigravity 瀹夎鐘舵€侊紝姝ｅ湪娓呯悊鎭㈠鈥n'
  sudo env DEBIAN_FRONTEND=noninteractive dpkg --remove --force-remove-reinstreq --force-depends antigravity </dev/null || true
fi
sudo env DEBIAN_FRONTEND=noninteractive dpkg --configure -a </dev/null || true
for dup_base in antigravity claude-desktop chatgpt; do
  if [ -f "/etc/apt/sources.list.d/${dup_base}.list" ] && [ -f "/etc/apt/sources.list.d/${dup_base}.sources" ]; then
    sudo rm -f "/etc/apt/sources.list.d/${dup_base}.sources"
  fi
done
printf '姝ｅ湪鍑嗗瀹夎渚濊禆鈥n'
apt_get update
apt_get install -y --no-install-recommends -f wmctrl ca-certificates curl gnupg jq dbus-x11 xauth x11-utils x11-xserver-utils tigervnc-standalone-server tigervnc-tools openbox gnome-keyring gcr bubblewrap socat openssh-client libglib2.0-bin ibus ibus-gtk3 gir1.2-ibus-1.0 gir1.2-gtk-3.0 python3-gi python3-pil python3-xlib xsettingsd xdg-utils

printf '姝ｅ湪鏌ヨ Google Antigravity 瀹樻柟鏈€鏂扮増鏈€n'
release_id=""
for index_url in "https://antigravity.google/docs/getting-started.md" "https://antigravity.google/download/"; do
  if page="$(cage_curl_download - --proto '=https' --proto-redir '=https' -fsSL --connect-timeout 15 "$index_url" </dev/null 2>/dev/null)"; then
    candidate="$(printf '%s' "$page" | grep -Eo 'https://storage\.googleapis\.com/antigravity-public/antigravity-hub/[0-9]+\.[0-9]+\.[0-9]+-[0-9]+/' | head -n 1 | sed -E 's#.*/antigravity-hub/([0-9]+\.[0-9]+\.[0-9]+-[0-9]+)/#\1#' || true)"
    if [[ "$candidate" =~ ^[0-9]+\.[0-9]+\.[0-9]+-[0-9]+$ ]]; then
      release_id="$candidate"
      break
    fi
  fi
done
if [ -z "$release_id" ]; then
  release_id="$fallback_release"
fi
wanted_version="${release_id%%-*}"

installed_status="$(dpkg-query -W -f='${Status}' antigravity 2>/dev/null || true)"
installed_version="$(dpkg-query -W -f='${Version}' antigravity 2>/dev/null || true)"
if [ "$installed_status" = 'install ok installed' ] && [ -n "$installed_version" ] && dpkg --compare-versions "$installed_version" ge "$wanted_version"; then
  xdg-mime default workbench-web.desktop x-scheme-handler/http x-scheme-handler/https 2>/dev/null || true
  printf 'Antigravity Desktop 宸叉槸鏈€鏂扮増鏈?(%s)銆俓n' "$installed_version"
  exit 0
fi

expected_size=""
if [ "$release_id" != "$fallback_release" ]; then
  meta_url="https://storage.googleapis.com/storage/v1/b/antigravity-public/o/antigravity-hub%2F${release_id}%2F${deb_arch}%2FAntigravity.deb"
  if meta_json="$(cage_curl_download - --proto '=https' --proto-redir '=https' -fsSL --connect-timeout 15 "$meta_url" </dev/null 2>/dev/null)"; then
    expected_size="$(printf '%s' "$meta_json" | jq -er 'select(.bucket == "antigravity-public") | .size | select(type == "string" and test("^[1-9][0-9]{6,10}$"))' 2>/dev/null || true)"
  fi
  if [ -z "$expected_size" ]; then
    printf '瀹樻柟鏈€鏂扮増 (%s) 鐨?Debian 瀹夎鍖呭厓鏁版嵁鏆備笉鍙敤锛屽洖閫€鑷冲熀鍑嗙増鏈€n' "$wanted_version"
    release_id="$fallback_release"
    wanted_version="${release_id%%-*}"
    if [ "$installed_status" = 'install ok installed' ] && [ -n "$installed_version" ] && dpkg --compare-versions "$installed_version" ge "$wanted_version"; then
      xdg-mime default workbench-web.desktop x-scheme-handler/http x-scheme-handler/https 2>/dev/null || true
      printf 'Antigravity Desktop 宸插畨瑁?(%s)銆俓n' "$installed_version"
      exit 0
    fi
  fi
fi

verify_deb() {
  local file="$1" pkg ver pkg_arch actual_size
  [ -f "$file" ] || return 1
  if [ "$release_id" = "$fallback_release" ]; then
    printf '%s  %s\n' "$fallback_sha256" "$file" | sha256sum --check --status 2>/dev/null || return 1
  else
    actual_size="$(wc -c < "$file" | tr -d '[:space:]')"
    [ "$actual_size" = "$expected_size" ] || return 1
  fi
  pkg="$(dpkg-deb -f "$file" Package 2>/dev/null || true)"
  ver="$(dpkg-deb -f "$file" Version 2>/dev/null || true)"
  pkg_arch="$(dpkg-deb -f "$file" Architecture 2>/dev/null || true)"
  [ "$pkg" = "antigravity" ] && [ "$ver" = "$wanted_version" ] && [ "$pkg_arch" = "$arch" ]
}

cache_dir="$HOME/.cache/claude-cage"
mkdir -p "$cache_dir"
cached_deb="$cache_dir/Antigravity-${wanted_version}-${deb_arch}.deb"
if verify_deb "$cached_deb"; then
  printf '姝ｅ湪澶嶇敤宸叉牎楠岀殑 Google Antigravity (%s) 瀹樻柟瀹夎鍖呯紦瀛樷€n' "$wanted_version"
else
  temporary=$(mktemp -d)
  trap 'rm -rf "$temporary"' EXIT
  printf '姝ｅ湪涓嬭浇骞舵牎楠?Google Antigravity (%s) 瀹樻柟瀹夎鍖呪€n' "$wanted_version"
  deb_url="https://storage.googleapis.com/antigravity-public/antigravity-hub/${release_id}/${deb_arch}/Antigravity.deb"
  cage_curl_download "$temporary/Antigravity.deb" --proto '=https' --proto-redir '=https' -fSL --connect-timeout 15 "$deb_url" -o "$temporary/Antigravity.deb"
  verify_deb "$temporary/Antigravity.deb" || { echo 'Antigravity package verification failed; installation stopped.' >&2; exit 1; }
  mv -f "$temporary/Antigravity.deb" "$cached_deb"
  for old_deb in "$cache_dir"/Antigravity-*-"${deb_arch}".deb; do
    if [ -f "$old_deb" ] && [ "$old_deb" != "$cached_deb" ]; then
      rm -f "$old_deb"
    fi
  done
fi
chmod 0644 "$cached_deb"
printf '姝ｅ湪瀹夎 Antigravity Desktop (%s)鈥n' "$wanted_version"
apt_get install -y --no-install-recommends "$cached_deb"
if [ -f /etc/apt/sources.list.d/antigravity.list ] && [ -f /etc/apt/sources.list.d/antigravity.sources ]; then
  sudo rm -f /etc/apt/sources.list.d/antigravity.sources
fi
test "$(dpkg-query -W -f='${Status}' antigravity)" = 'install ok installed'
xdg-mime default workbench-web.desktop x-scheme-handler/http x-scheme-handler/https 2>/dev/null || true
printf 'Antigravity Desktop (%s) 宸插畨瑁呫€俓n' "$(dpkg-query -W -f='${Version}' antigravity)"
