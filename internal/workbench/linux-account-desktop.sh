#!/bin/bash
# Runs as the workspace user. The caller supplies a validated account ID.
set -euo pipefail
umask 077
id=$1
code=$2
scope=$3
provider=${4:-claude}
[[ $provider == claude || $provider == antigravity || $provider == codex ]]
if [[ $id == default && $scope == delete ]]; then
  state=$(systemctl show --property=ActiveState --value cage-claude-desktop.service 2>/dev/null || :)
  if [[ $state != "" && $state != inactive && $state != failed ]]; then
    sudo systemctl stop cage-claude-desktop.service 2>/dev/null || ! systemctl is-active --quiet cage-claude-desktop.service
  fi
  sudo systemctl reset-failed cage-claude-desktop.service 2>/dev/null || :
  if [[ -e /run/systemd/system/cage-claude-desktop.service.d/40-account.conf || -e /run/systemd/system/cage-claude-desktop.service.d/account-scope ]]; then
    sudo rm -f -- /run/systemd/system/cage-claude-desktop.service.d/40-account.conf /run/systemd/system/cage-claude-desktop.service.d/account-scope 2>/dev/null || :
  fi
  for target in "$HOME/.config/Claude" "$HOME/.cache/Claude" "$HOME/.local/share/Claude" "$HOME/.local/share/keyrings"; do
    if [[ -e $target || -L $target ]]; then
      chmod -R u+rwX -- "$target" 2>/dev/null || :
      rm -rf -- "$target"
    fi
  done
  exit 0
fi
[[ $id =~ ^[0-9a-f]{24}$ && ( $code == default || $code =~ ^[0-9a-f]{24}$ ) ]]
unit="desktop-$id"
base="$HOME/.local/share/desktop-profiles/$id"
runtime="/run/$unit"
if [[ $scope == delete ]]; then
  if [[ -d $base ]]; then
    exec 9>"$base/launch.lock"
    flock -w 55 9
  fi
  state=$(systemctl show --property=ActiveState --value "$unit.service" 2>/dev/null || :)
  if [[ $state != "" && $state != inactive && $state != failed ]]; then
    sudo systemctl stop "$unit.service" 2>/dev/null || ! systemctl is-active --quiet "$unit.service"
  fi
  sudo systemctl reset-failed "$unit.service" 2>/dev/null || :
  if [[ -d $base ]]; then
    exec 9>&-
    chmod -R u+rwX -- "$base" 2>/dev/null || :
    rm -rf -- "$base"
  fi
  exit 0
fi
mkdir -p "$base"
exec 9>"$base/launch.lock"
flock -w 55 9
if [[ $scope == stop ]]; then
  if systemctl is-active --quiet "$unit.service"; then
    sudo systemctl stop "$unit.service" 2>/dev/null || ! systemctl is-active --quiet "$unit.service"
  fi
  exit 0
fi
if systemctl is-active --quiet "$unit.service"; then exit 0; fi
case "$provider" in
  claude) test -x /usr/bin/claude-desktop ;;
  antigravity) test -x /usr/bin/antigravity ;;
  codex) test -x /usr/bin/chatgpt ;;
esac
printf '%s\n' "$provider" > "$base/provider"
sudo systemctl reset-failed "$unit.service" 2>/dev/null || :
# The controller creates node namespaces. Older administrator-managed
# namespaces keep their Desktop-only binding until the account is changed.
props=("--property=EnvironmentFile=-/etc/claude-cage/environment")
if [[ $scope == dedicated || $scope == node-* ]]; then
  namespace=$unit
  if [[ $scope == node-* ]]; then route=${scope#node-}; [[ $route =~ ^[0-9a-f]{24}$ ]]; namespace="net-$route"; fi
  test -e "/run/netns/$namespace" || { echo '专属网络尚未就绪'; exit 1; }
  props+=("--property=NetworkNamespacePath=/run/netns/$namespace")
  [[ -s /etc/netns/$namespace/resolv.conf ]] || { echo '专属网络尚未就绪'; exit 1; }
  props+=("--property=BindReadOnlyPaths=/etc/netns/$namespace/resolv.conf:/etc/resolv.conf"
          "--property=InaccessiblePaths=-/run/systemd/resolve/io.systemd.Resolve -/run/dbus/system_bus_socket -/run/nscd/socket")
elif [[ $scope != shared ]]; then exit 2; fi
if [[ "${CAGE_ACCOUNT_TZ:-}" =~ ^[A-Za-z0-9_/+.-]{1,64}$ && "${CAGE_ACCOUNT_TZ}" != *..* ]]; then
  props+=("--property=Environment=account_tz=${CAGE_ACCOUNT_TZ}")
fi
if [[ "${CAGE_ACCOUNT_LANGUAGE:-}" =~ ^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$ ]]; then
  props+=("--property=Environment=account_language=${CAGE_ACCOUNT_LANGUAGE}")
fi
program=$(mktemp "$base/session.XXXXXX")
trap 'rm -f -- "$program"' EXIT
cat >"$program" <<'SESSION'
set -euo pipefail
umask 077
base=$1
code=$2
provider=${3:-claude}
data="$base/data"
if [[ $provider == claude ]]; then
  if [[ $code == default ]]; then
    config="$base/code"
  else
    config="$HOME/.local/share/cage/claude-accounts/$code/config"
  fi
  mkdir -p "$config"
  if [[ $code != default ]]; then
    /usr/local/bin/cage-claude-account share "$code" >/dev/null || :
  fi
  export CLAUDE_CONFIG_DIR
  CLAUDE_CONFIG_DIR=$(python3 -I -c 'import runpy,sys;print(runpy.run_path("/usr/local/bin/client_runtime.py")["path"](sys.argv[1]))' "$config")
elif [[ $provider == antigravity ]]; then
  mkdir -p "$base/home/.gemini/config"
  export ANTIGRAVITY_GEMINI_DIR
  ANTIGRAVITY_GEMINI_DIR=$(python3 -I -c 'import runpy,sys;print(runpy.run_path("/usr/local/bin/client_runtime.py")["path"](sys.argv[1]))' "$base/home/.gemini")
elif [[ $provider == codex ]]; then
  mkdir -p "$base/home/.codex"
  export CODEX_HOME="$base/home/.codex"
fi
work_home=$HOME
export HOME="$base/home" XDG_RUNTIME_DIR="$RUNTIME_DIRECTORY"
unset RUNTIME_DIRECTORY INVOCATION_ID JOURNAL_STREAM SYSTEMD_EXEC_PID
rm -f "$XDG_RUNTIME_DIR/stopping"
app=
# shellcheck disable=SC2329
on_term() {
  if [[ -n "$app" ]]; then
    kill -TERM "$app" 2>/dev/null || :
  fi
  if [[ -f "$XDG_RUNTIME_DIR/stopping" ]]; then
    rm -f "$XDG_RUNTIME_DIR/stopping"
    exit 0
  fi
  trap - TERM
  kill -TERM "$$"
}
trap on_term TERM
export PATH=/usr/bin:/bin XDG_CURRENT_DESKTOP=Openbox
mkdir -p "$HOME" "$data" "$HOME/.local/share/applications" "$HOME/.config"
# Give the login browser the same profile, network namespace and callback
# handler as its originating Desktop. gio reads these defaults in this HOME.
python3 - "$base" "$provider" <<'PY'
from pathlib import Path
import sys
from configparser import ConfigParser
base=Path(sys.argv[1])
provider=sys.argv[2] if len(sys.argv) > 2 else 'claude'
data=base/'data'
apps=Path.home()/'.local/share/applications'
import re
def arg(path):
    # xdg-utils splits Exec on whitespace and keeps quote marks. Quote only
    # when needed, so the common path also survives that fallback parser.
    if re.fullmatch(r'[A-Za-z0-9_@+=:,./-]+', str(path)):
        return str(path)
    return '"'+str(path).replace('\\','\\\\').replace('"','\\"').replace('$','\\$').replace('`','\\`').replace('%','%%')+'"'
cache = ConfigParser(interpolation=None)
cache.optionxform = str
cache.read(apps/'mimeinfo.cache')
if 'MIME Cache' not in cache:
    cache.add_section('MIME Cache')
if provider == 'antigravity':
    app_entry = ('com.google.Antigravity', ['antigravity'], '/usr/bin/antigravity '+arg('--user-data-dir='+str(data))+' %u')
elif provider == 'codex':
    app_entry = ('com.openai.ChatGPT', ['chatgpt', 'codex', 'openai'], '/usr/bin/env '+arg('CODEX_HOME='+str(base/'home'/'.codex'))+' /usr/bin/chatgpt '+arg('--user-data-dir='+str(data))+' %u')
else:
    app_entry = ('com.anthropic.Claude', ['claude'], '/usr/bin/claude-desktop '+arg('--user-data-dir='+str(data))+' %u')
name, schemes, command = app_entry
entries = [
    (name, ['x-scheme-handler/'+scheme for scheme in schemes], command),
    ('web', ['x-scheme-handler/http','x-scheme-handler/https'], '/usr/bin/python3 -I /usr/local/lib/desktop-auth-browser.py %u'),
    ('cage-project-folder', ['inode/directory'], '/usr/local/bin/cage-desktop-files --open %f'),
]
default_lines = ['[Default Applications]']
for name,mimes,line in entries:
    (apps/(name+'.desktop')).write_text('[Desktop Entry]\nType=Application\nName='+name+'\nExec='+line+'\nMimeType='+';'.join(mimes)+';\n')
    for mime in mimes:
        cache['MIME Cache'][mime]=name+'.desktop;'
        default_lines.append(mime+'='+name+'.desktop;')
with (apps/'mimeinfo.cache').open('w') as output:
    cache.write(output,space_around_delimiters=False)
(Path.home()/'.config/mimeapps.list').write_text('\n'.join(default_lines)+'\n')
PY
# Use the desktop-entry parser for quoted arguments when the browser opens a
# custom URI. Route local files and folders to this instance's file broker.
# /run is mounted noexec, so the opener lives in the private profile. A
# skipped opener made the login callback start a Desktop without this
# instance's data folder instead of returning to the one that signed in.
opener="$base/bin"
mkdir -p "$opener"
cat > "$opener/xdg-open" <<'OPEN'
#!/bin/sh
case "${1:-}" in
  http://*|https://*|claude://*|antigravity://*|chatgpt://*|codex://*|openai://*|about:blank) exec /usr/bin/gio open "$1" ;;
  *) exec /usr/local/bin/cage-desktop-files --open "${1:-}" ;;
esac
OPEN
chmod 0700 "$opener/xdg-open"
export PATH="$opener:$PATH" BROWSER="$opener/xdg-open"
export XAUTHORITY="$XDG_RUNTIME_DIR/Xauthority"
touch "$XAUTHORITY"
cookie=$(mcookie)
# -displayfd picks a free X display under concurrent launches.
xauth -f "$XAUTHORITY" add :0 . "$cookie"
od -An -N4 -tx1 /dev/urandom | tr -d ' \n' > "$XDG_RUNTIME_DIR/password"
tigervncpasswd -f < "$XDG_RUNTIME_DIR/password" > "$XDG_RUNTIME_DIR/vnc-password"
# The viewer sets the density of the screen showing it through the settings
# manager; until then the configured one applies. Record both for the viewer.
display_scale=2
if [[ -r /etc/claude-cage/display-scale && $(< /etc/claude-cage/display-scale) == 1.5 ]]; then
  display_scale=1.5
fi
printf '%s\n' "$display_scale" > "$XDG_RUNTIME_DIR/display-scale"
printf '%s\n' "$display_scale" > "$XDG_RUNTIME_DIR/base-display-scale"
dpi=$(awk -v scale="$display_scale" 'BEGIN {printf "%.0f", scale * 96 * 1024}')
# Start with the logical size an earlier version had at 96 DPI.
geometry=$(awk -v scale="$display_scale" 'BEGIN {printf "%dx%d", 1280 * scale, 800 * scale}')
Xtigervnc -displayfd 3 3>"$XDG_RUNTIME_DIR/display" -geometry "$geometry" -depth 24 -rfbport -1 -rfbunixpath "$XDG_RUNTIME_DIR/vnc.sock" -rfbunixmode 0600 -auth "$XAUTHORITY" -nolisten tcp -SecurityTypes VncAuth -PasswordFile "$XDG_RUNTIME_DIR/vnc-password" &
for _ in {1..100}; do test -s "$XDG_RUNTIME_DIR/display" && break; sleep .05; done
export DISPLAY=":$(cat "$XDG_RUNTIME_DIR/display")"
xauth -f "$XAUTHORITY" add "$DISPLAY" . "$cookie"
if [[ -n "${account_tz:-}" ]]; then export TZ=$account_tz; fi
if [[ -n "${account_language:-}" ]]; then CAGE_LANGUAGE=$account_language; fi
unset account_tz account_language
desktop_lang=""
if [[ "${CAGE_LANGUAGE:-}" =~ ^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$ ]]; then
  desktop_lang="${CAGE_LANGUAGE}"
  printf '%s\n' "$desktop_lang" > "$XDG_RUNTIME_DIR/browser-language"
fi
unset CAGE_LANGUAGE
dbus-update-activation-environment DISPLAY XAUTHORITY HOME XDG_RUNTIME_DIR TZ LANG LC_ALL
if [ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ]; then
  printf '%s' "$DBUS_SESSION_BUS_ADDRESS" > "$XDG_RUNTIME_DIR/bus-address"
fi
cat > "$XDG_RUNTIME_DIR/windows.xml" <<'XML'
<openbox_config xmlns="http://openbox.org/3.4/rc"><desktops><number>1</number></desktops><applications><application type="normal"><decor>no</decor><maximized>yes</maximized></application></applications></openbox_config>
XML
openbox --config-file "$XDG_RUNTIME_DIR/windows.xml" >/dev/null 2>&1 &
# Settings that stay as they are when the density changes. Electron draws its
# own GTK controls; Openbox decorations alone do not hide them.
printf 'Gtk/DecorationLayout ":"\n' > "$XDG_RUNTIME_DIR/xsettings.fixed"
guest_theme=
if [ -r "$work_home/.config/claude-cage/theme-variant" ]; then
  read -r guest_theme < "$work_home/.config/claude-cage/theme-variant" || true
  if [ "$guest_theme" = dark ]; then
    printf 'Net/ThemeName "Adwaita-dark"\nGtk/ApplicationPreferDarkTheme 1\n' >> "$XDG_RUNTIME_DIR/xsettings.fixed"
  elif [ "$guest_theme" = light ]; then
    printf 'Net/ThemeName "Adwaita"\nGtk/ApplicationPreferDarkTheme 0\n' >> "$XDG_RUNTIME_DIR/xsettings.fixed"
  fi
fi
if [ -n "$guest_theme" ]; then
  if [ -x /usr/local/bin/cage-apply-theme ]; then
    HOME="$work_home" /usr/local/bin/cage-apply-theme "$guest_theme" >/dev/null 2>&1 || true
  fi
  if command -v gsettings >/dev/null 2>&1; then
    if [ "$guest_theme" = dark ]; then
      gsettings set org.gnome.desktop.interface color-scheme prefer-dark 2>/dev/null || true
      gsettings set org.gnome.desktop.interface gtk-theme Adwaita-dark 2>/dev/null || true
    elif [ "$guest_theme" = light ]; then
      gsettings set org.gnome.desktop.interface color-scheme prefer-light 2>/dev/null || true
      gsettings set org.gnome.desktop.interface gtk-theme Adwaita 2>/dev/null || true
    fi
  fi
fi
{ printf 'Xft/DPI %s\nGdk/WindowScalingFactor 1\nGdk/UnscaledDPI %s\n' "$dpi" "$dpi"; cat "$XDG_RUNTIME_DIR/xsettings.fixed"; } > "$XDG_RUNTIME_DIR/xsettings.conf"
xsettingsd --config="$XDG_RUNTIME_DIR/xsettings.conf" &
printf '%s\n' "$!" > "$XDG_RUNTIME_DIR/xsettings.pid"
python3 - <<'PYSETTINGS'
from Xlib import display
import time
d = display.Display()
selection = d.intern_atom('_XSETTINGS_S0')
for _ in range(60):
    if d.get_selection_owner(selection):
        break
    time.sleep(.05)
else:
    raise SystemExit('Desktop display settings unavailable')
d.close()
PYSETTINGS
mkfifo "$XDG_RUNTIME_DIR/unlock"
gnome-keyring-daemon --unlock --components=secrets < "$XDG_RUNTIME_DIR/unlock" >/dev/null
rm "$XDG_RUNTIME_DIR/unlock"
for _ in {1..60}; do
  gdbus call --session --dest org.freedesktop.secrets --object-path /org/freedesktop/secrets/aliases/default --method org.freedesktop.DBus.Properties.Get org.freedesktop.Secret.Collection Locked 2>/dev/null | grep -q false && break
  sleep .1
done
gdbus call --session --dest org.freedesktop.secrets --object-path /org/freedesktop/secrets/aliases/default --method org.freedesktop.DBus.Properties.Get org.freedesktop.Secret.Collection Locked | grep -q false
if [[ -x /usr/local/bin/cage-desktop-files ]]; then
  /usr/local/bin/cage-desktop-files &
  files_pid=$!
  for _ in {1..100}; do
    kill -0 "$files_pid"
    if [[ -S "$XDG_RUNTIME_DIR/files.sock" ]]; then break; fi
    sleep .1
  done
  test -S "$XDG_RUNTIME_DIR/files.sock"
  export GTK_USE_PORTAL=1
fi
notify=$NOTIFY_SOCKET
unset NOTIFY_SOCKET
cd "$HOME"
input_session=()
if [[ -x /usr/local/bin/cage-input-session ]]; then input_session=(/usr/local/bin/cage-input-session); fi
lang_args=()
if [[ -n "${desktop_lang:-}" ]]; then lang_args=("--lang=${desktop_lang}"); fi
client_runtime=()
if [[ -r /usr/local/bin/client_runtime.py ]]; then client_runtime=(/usr/bin/python3 -I /usr/local/bin/client_runtime.py); fi
if [[ $provider == antigravity ]]; then
  ${input_session[@]+"${input_session[@]}"} ${client_runtime[@]+"${client_runtime[@]}"} antigravity "--user-data-dir=$data" ${lang_args[@]+"${lang_args[@]}"} --disable-quic --force-webrtc-ip-handling-policy=disable_non_proxied_udp --password-store=gnome-libsecret --xdg-portal-required-version=3 > >(/usr/bin/python3 -I /usr/local/lib/desktop-identity.py "$XDG_RUNTIME_DIR/account.json" --antigravity) &
elif [[ $provider == codex ]]; then
  ${input_session[@]+"${input_session[@]}"} ${client_runtime[@]+"${client_runtime[@]}"} /usr/bin/env "CODEX_HOME=$CODEX_HOME" /usr/bin/chatgpt "--user-data-dir=$data" ${lang_args[@]+"${lang_args[@]}"} --disable-quic --force-webrtc-ip-handling-policy=disable_non_proxied_udp --password-store=gnome-libsecret --xdg-portal-required-version=3 > >(/usr/bin/python3 -I /usr/local/lib/desktop-identity.py "$XDG_RUNTIME_DIR/account.json" --codex) &
else
  DESKTOP_LOG_LEVEL=debug CLAUDE_ENABLE_LOGGING=1 DEBUG='*current-account*' ${input_session[@]+"${input_session[@]}"} ${client_runtime[@]+"${client_runtime[@]}"} claude-desktop "--user-data-dir=$data" ${lang_args[@]+"${lang_args[@]}"} --password-store=gnome-libsecret --xdg-portal-required-version=3 > >(/usr/bin/python3 -I /usr/local/lib/desktop-identity.py "$XDG_RUNTIME_DIR/account.json") &
fi
app=$!
printf '%s\n' "$app" > "$XDG_RUNTIME_DIR/app-pid"
for _ in {1..250}; do
  if ! kill -0 "$app" 2>/dev/null; then
    status=0
    wait "$app" || status=$?
    if [[ -f "$XDG_RUNTIME_DIR/stopping" ]]; then
      rm -f "$XDG_RUNTIME_DIR/stopping"
      exit 0
    fi
    if [[ "$status" -ne 0 ]]; then
      exit "$status"
    fi
    exit 1
  fi
  if xprop -root _NET_CLIENT_LIST 2>/dev/null | grep -q '0x[1-9a-f]'; then
    NOTIFY_SOCKET="$notify" systemd-notify --ready
    status=0
    wait "$app" || status=$?
    if [[ -f "$XDG_RUNTIME_DIR/stopping" ]]; then
      rm -f "$XDG_RUNTIME_DIR/stopping"
      exit 0
    fi
    exit "$status"
  fi
  sleep .1
done
if [[ -f "$XDG_RUNTIME_DIR/stopping" ]]; then
  rm -f "$XDG_RUNTIME_DIR/stopping"
  exit 0
fi
exit 1
SESSION
user=$(id -un)
sudo systemd-run --quiet --no-block --collect --unit="$unit" --service-type=notify \
  --property="User=$user" --property="RuntimeDirectory=$unit" --property=RuntimeDirectoryMode=0700 \
  --property=NotifyAccess=all --property=TimeoutStartSec=45 --property=TimeoutStopSec=8 \
  --property="ExecStop=/bin/bash /usr/local/lib/desktop-finish.sh $runtime $unit.service" \
  --property=KillMode=control-group --property=PrivateTmp=yes ${props[@]+"${props[@]}"} \
  /usr/bin/dbus-run-session -- /bin/bash "$program" "$base" "$code" "$provider"
ok=
trap 'if [[ -z "$ok" ]]; then sudo systemctl stop "$unit.service" >/dev/null 2>&1 || :; fi; rm -f -- "$program"' EXIT
for _ in {1..160}; do test -p "$runtime/unlock" && break; sleep .1; done
test -p "$runtime/unlock"
# shellcheck disable=SC2016
timeout 5 sh -c 'cat >"$1/unlock"' sh "$runtime"
for _ in {1..260}; do
  if systemctl is-active --quiet "$unit.service"; then ok=1; exit 0; fi
  sleep .1
done
exit 1
