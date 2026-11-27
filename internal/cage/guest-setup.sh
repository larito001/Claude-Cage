#!/usr/bin/env bash
set -euo pipefail
sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get install -y --no-install-recommends locales tzdata iproute2 iptables tigervnc-standalone-server openbox xdg-utils x11-xserver-utils tigervnc-tools novnc websockify fonts-noto-cjk fonts-noto-color-emoji python3-xlib xsettingsd pulseaudio pulseaudio-utils xdotool libgtk-3-0 ibus ibus-gtk3 gir1.2-ibus-1.0 gir1.2-gtk-3.0 python3-gi
if ! id cage-browser >/dev/null 2>&1; then
  sudo useradd --create-home --shell /bin/bash cage-browser
fi
python3 - <<'CAGE_BROWSER_ACCOUNT'
import os, pwd
account = pwd.getpwnam('cage-browser')
if account.pw_uid < 1000 or account.pw_uid == os.getuid() or account.pw_dir != '/home/cage-browser':
    raise SystemExit('The browser requires its own ordinary account and home directory.')
CAGE_BROWSER_ACCOUNT
