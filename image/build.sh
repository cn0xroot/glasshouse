#!/usr/bin/env bash
# Build a Glasshouse base image from Canonical's official Ubuntu 26.04 LTS cloud image.
#
#   CC_MONITOR_REF=<40-hex commit> ./image/build.sh [--arch amd64|arm64] [--profile headless|desktop] [--format qcow2|raw]
#                                                   [--mirror auto|official|tuna|ustc] [--keep-mirror]
#
# Mirrors (for builds in mainland China): --mirror (or GLASSHOUSE_MIRROR) picks where the cloud image and the guest's
# APT packages come from. "auto" (default) probes Canonical's servers, Tsinghua (tuna) and USTC and only leaves the
# official servers when they are unreachable or clearly slower; the others force one source (falling back to the
# official one if it fails). Whatever the source, SHA256SUMS is verified against Canonical's GPG key and the image against
# that signed list, and apt verifies its own signatures, so a mirror does not have to be trusted. The APT source is
# switched back to the official one at the end of the build (--keep-mirror leaves the mirror in the image; add its host to
# network.allow if you use the allowlist network mode).
#
# Optional environment:
#   APT_MIRROR=<https://host/ubuntu>   use this APT mirror in the build guest instead of the selected one
#   NPM_REGISTRY=<https://registry>    npm registry for CC-Monitor's dependencies and Claude Code (e.g. https://registry.npmmirror.com)
#   CC_MONITOR_REPO=<https://git url>  where to clone CC-Monitor from (the pinned commit hash is still verified)
#   CLAUDE_CODE_VERSION=latest|stable|x.y.z   Claude Code is installed at the newest release by default; the version that
#                                      ended up in the image is printed and recorded in /etc/glasshouse-claude-code-version
#   CCSTATUSLINE_VERSION=latest|x.y.z  ccstatusline (https://github.com/sirmalloc/ccstatusline) is installed globally and
#                                      configured from image/ccstatusline/ (a copy of the host's own setup)
#
# Needs (Linux host, same CPU architecture as the image): qemu-img, gpgv,
# libguestfs-tools (virt-customize, virt-sysprep), isc-dhcp-client, git, curl, sha256sum.
# macOS guests need a *raw* arm64 image: build it on the Mac itself with image/build-mac.sh (Ubuntu's arm64 cloud
# image, customized inside a Virtualization.framework VM), or here on an arm64 Linux machine with --format raw and
# copy it (plus its .sha256) to the Mac's $GLASSHOUSE_HOME/images/.
set -euo pipefail

ARCH="$(uname -m)"; case "$ARCH" in x86_64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; esac
PROFILE=desktop
FORMAT=qcow2
MIRROR=""
KEEP_MIRROR=0
while [ $# -gt 0 ]; do
  case "$1" in
    --arch) ARCH="$2"; shift 2;;
    --profile) PROFILE="$2"; shift 2;;
    --format) FORMAT="$2"; shift 2;;
    --mirror) MIRROR="$2"; shift 2;;
    --keep-mirror) KEEP_MIRROR=1; shift;;
    -h|--help) awk 'NR>1 && /^set -euo/{exit} NR>1{print}' "$0"; exit 0;;
    *) echo "unknown option $1" >&2; exit 2;;
  esac
done
case "$ARCH" in amd64|arm64) ;; *) echo "bad --arch" >&2; exit 2;; esac
case "$PROFILE" in headless|desktop) ;; *) echo "bad --profile" >&2; exit 2;; esac
case "$FORMAT" in qcow2|raw) ;; *) echo "bad --format" >&2; exit 2;; esac

HOST_ARCH="$(uname -m)"; case "$HOST_ARCH" in x86_64) HOST_ARCH=amd64;; aarch64|arm64) HOST_ARCH=arm64;; esac
[ "$(uname -s)" = Linux ] || { echo "build.sh needs a Linux host (libguestfs); on a Mac use image/build-mac.sh" >&2; exit 1; }
[ "$ARCH" = "$HOST_ARCH" ] || { echo "cannot build a $ARCH image on a $HOST_ARCH host (no emulation)" >&2; exit 1; }

# Third-party guard code is pinned to an exact commit: no moving branches.
: "${CC_MONITOR_REF:?set CC_MONITOR_REF to a 40-hex commit of github.com/cn0xroot/CC-Monitor (review it first)}"
[[ "$CC_MONITOR_REF" =~ ^[0-9a-f]{40}$ ]] || { echo "CC_MONITOR_REF must be a full 40-character commit hash" >&2; exit 1; }

for t in qemu-img gpgv git curl sha256sum virt-customize virt-sysprep; do
  command -v "$t" >/dev/null || { echo "missing tool: $t" >&2; exit 1; }
done
KEYRING=/usr/share/keyrings/ubuntu-cloudimage-keyring.gpg
[ -f "$KEYRING" ] || { echo "missing $KEYRING (apt install ubuntu-cloudimage-keyring)" >&2; exit 1; }

HOME_DIR="${GLASSHOUSE_HOME:-$HOME/.local/share/glasshouse}"
OUT_DIR="$HOME_DIR/images"; CACHE="$HOME_DIR/cache"
mkdir -p "$OUT_DIR" "$CACHE"; chmod 700 "$HOME_DIR"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

# ---- download sources ------------------------------------------------------------------------------------------
MIRROR="${MIRROR:-${GLASSHOUSE_MIRROR:-auto}}"
case "$MIRROR" in auto|official|tuna|ustc) ;; *) echo "bad --mirror (auto|official|tuna|ustc)" >&2; exit 2;; esac

URL_RE='^https?://[A-Za-z0-9._:/-]+$'
[[ -z "${APT_MIRROR:-}" || "$APT_MIRROR" =~ $URL_RE ]] || { echo "APT_MIRROR must be an http(s) URL such as https://mirrors.tuna.tsinghua.edu.cn/ubuntu" >&2; exit 1; }
NPM_REGISTRY="${NPM_REGISTRY:-}"
[[ -z "$NPM_REGISTRY" || "$NPM_REGISTRY" =~ $URL_RE ]] || { echo "NPM_REGISTRY must be an http(s) URL" >&2; exit 1; }
NPM_ARG=""; [ -z "$NPM_REGISTRY" ] || NPM_ARG="--registry=$NPM_REGISTRY"
CC_MONITOR_REPO="${CC_MONITOR_REPO:-https://github.com/cn0xroot/CC-Monitor.git}"
[[ "$CC_MONITOR_REPO" =~ ^https://[A-Za-z0-9._:/@~+-]+$ ]] || { echo "CC_MONITOR_REPO must be an https git URL" >&2; exit 1; }

# source name -> directory holding SHA256SUMS(.gpg) and the image / image file name there / APT mirror base
src_dir() { case "$1" in
  official) echo "https://cloud-images.ubuntu.com/releases/26.04/release";;
  tuna)     echo "https://mirrors.tuna.tsinghua.edu.cn/ubuntu-cloud-images/resolute/current";;
  ustc)     echo "https://mirrors.ustc.edu.cn/ubuntu-cloud-images/resolute/current";;
esac; }
src_img() { case "$1" in official) echo "ubuntu-26.04-server-cloudimg-$ARCH.img";; *) echo "resolute-server-cloudimg-$ARCH.img";; esac; }
src_apt() { case "$1" in
  official) echo "";;
  tuna)     echo "https://mirrors.tuna.tsinghua.edu.cn/ubuntu";;
  ustc)     echo "https://mirrors.ustc.edu.cn/ubuntu";;
esac; }

# milliseconds to fetch the first 512 KiB of the image from a source; 99999 when that fails or takes more than 8 s
probe_ms() {
  local t
  t="$(curl -fsSL -r 0-524287 -o /dev/null -m 8 -w '%{time_total}' "$(src_dir "$1")/$(src_img "$1")" 2>/dev/null)" || { echo 99999; return; }
  awk -v t="$t" 'BEGIN{printf "%d", t*1000}'
}

CANDIDATES=()
case "$MIRROR" in
  official) CANDIDATES=(official);;
  tuna|ustc) CANDIDATES=("$MIRROR" official);;
  auto)
    for s in official tuna ustc; do ( probe_ms "$s" > "$WORK/probe.$s" ) & done; wait
    declare -A MS
    for s in official tuna ustc; do MS[$s]="$(cat "$WORK/probe.$s")"; done
    best=tuna; [ "${MS[ustc]}" -lt "${MS[tuna]}" ] && best=ustc
    pick=official
    # leave the official servers only when they are unreachable or a mirror is at least twice as fast
    if [ "${MS[official]}" -ge 99999 ] || [ $(( MS[$best] * 2 )) -lt "${MS[official]}" ]; then pick="$best"; fi
    [ "${MS[$pick]}" -lt 99999 ] || pick=official   # nothing answered: let the download report the real error
    echo "==> mirror: auto -> $pick (official ${MS[official]} ms, tuna ${MS[tuna]} ms, ustc ${MS[ustc]} ms; 99999 = unreachable)"
    CANDIDATES=("$pick"); for s in official tuna ustc; do [ "$s" = "$pick" ] || CANDIDATES+=("$s"); done;;
esac

# Try the sources in order until one yields a signature-verified image. Sets IMG and APT_SRC.
IMG=""; APT_SRC=official
fetch_image() {
  local s dir img
  for s in "${CANDIDATES[@]}"; do
    dir="$(src_dir "$s")"; img="$(src_img "$s")"
    echo "==> downloading and verifying $img (source: $s)"
    if ! { curl -fsSL -m 60 -o "$WORK/SHA256SUMS" "$dir/SHA256SUMS" && curl -fsSL -m 60 -o "$WORK/SHA256SUMS.gpg" "$dir/SHA256SUMS.gpg"; }; then
      echo "    $s: cannot fetch the checksum list; trying the next source" >&2; continue
    fi
    gpgv --keyring "$KEYRING" "$WORK/SHA256SUMS.gpg" "$WORK/SHA256SUMS" \
      || { echo "    $s: BAD SIGNATURE on SHA256SUMS; not using this source" >&2; continue; }
    grep " \*\?$img\$" "$WORK/SHA256SUMS" > "$WORK/SUM" \
      || { echo "    $s: $img is not in the signed list; trying the next source" >&2; continue; }
    if [ -f "$CACHE/$img" ] && ( cd "$CACHE" && sha256sum -c "$WORK/SUM" >/dev/null 2>&1 ); then
      echo "    cached copy is up to date"; IMG="$img"; APT_SRC="$s"; return 0
    fi
    rm -f "$CACHE/$img.part"
    if ! curl -fL --retry 3 -o "$CACHE/$img.part" "$dir/$img"; then
      rm -f "$CACHE/$img.part"; echo "    $s: download failed; trying the next source" >&2; continue
    fi
    mv "$CACHE/$img.part" "$CACHE/$img"
    if ! ( cd "$CACHE" && sha256sum -c "$WORK/SUM" ); then
      rm -f "$CACHE/$img"; echo "    $s: checksum mismatch; trying the next source" >&2; continue
    fi
    IMG="$img"; APT_SRC="$s"; return 0
  done
  return 1
}
fetch_image || { echo "no source could provide a verified $ARCH cloud image" >&2; exit 1; }

# APT mirror for the build guest: the one that matches the image source, unless APT_MIRROR overrides it.
APT_BASE="${APT_MIRROR:-$(src_apt "$APT_SRC")}"
[[ -z "$APT_BASE" || "$APT_BASE" =~ $URL_RE ]] || { echo "APT_MIRROR must be an http(s) URL such as https://mirrors.tuna.tsinghua.edu.cn/ubuntu" >&2; exit 1; }
APT_BASE="${APT_BASE%/}"
[ -z "$APT_BASE" ] || echo "==> APT source for the build guest: $APT_BASE"
if [ "$APT_SRC" != official ] && [ -z "$NPM_REGISTRY" ]; then
  echo "    tip: if the npm or GitHub downloads below are slow, set NPM_REGISTRY=https://registry.npmmirror.com and/or CC_MONITOR_REPO=<a GitHub mirror of CC-Monitor>"
fi

echo "==> fetching CC-Monitor @ $CC_MONITOR_REF"
git clone --quiet "$CC_MONITOR_REPO" "$WORK/cc-monitor"
git -C "$WORK/cc-monitor" checkout --quiet --detach "$CC_MONITOR_REF"
[ "$(git -C "$WORK/cc-monitor" rev-parse HEAD)" = "$CC_MONITOR_REF" ]
rm -rf "$WORK/cc-monitor/.git"

cp "$CACHE/$IMG" "$WORK/work.qcow2"

PKGS=git,build-essential,python3,python3-pip,python3-venv,nodejs,npm,curl,ca-certificates,openssh-server,socat,jq,ripgrep,unzip,sudo
DESKTOP_PKGS=ubuntu-desktop-minimal,gdm3,gnome-terminal,libgl1-mesa-dri,fonts-noto-cjk

cat > "$WORK/glasshouse-cc-monitor.service" <<'UNIT'
[Unit]
Description=CC-Monitor (audit/guard for Claude Code inside the Glasshouse guest)
After=cloud-final.service
[Service]
User=agent
WorkingDirectory=/opt/cc-monitor
Environment=CC_MONITOR_WEBUI_HOST=127.0.0.1
ExecStart=/opt/cc-monitor/start.sh
Restart=on-failure
[Install]
WantedBy=multi-user.target
UNIT

cat > "$WORK/99-glasshouse.cfg" <<'CFG'
datasource_list: [NoCloud, None]
CFG

echo "==> growing the root partition for package installs"
# The stock cloud image's root filesystem is only a few GB. Grow it IN PLACE: virt-resize renumbers and moves the
# partitions, which breaks GRUB's BIOS boot record and leaves the image stuck at "Booting from Hard Disk...".
# The final image is sparse/compressed, and the guest grows its disk to policy vm.disk on first boot.
if [ "$PROFILE" = desktop ]; then BUILD_GB=20; else BUILD_GB=8; fi
qemu-img resize -q "$WORK/work.qcow2" "${BUILD_GB}G"
ROOT_INFO="$(guestfish --ro -a "$WORK/work.qcow2" <<'GF'
run
part-get-gpt-guid /dev/sda 1
part-get-gpt-type /dev/sda 1
part-get-name /dev/sda 1
part-list /dev/sda
GF
)"
ROOT_GUID="$(sed -n 1p <<<"$ROOT_INFO")"; ROOT_TYPE="$(sed -n 2p <<<"$ROOT_INFO")"; ROOT_NAME="$(sed -n 3p <<<"$ROOT_INFO")"
# the root partition is the last one on disk; keep its start sector, extend its end to the last usable sector
ROOT_START="$(awk '/part_num: 1$/{f=1} f&&/part_start:/{print $2/512; exit}' <<<"$ROOT_INFO")"
[[ "$ROOT_START" =~ ^[0-9]+$ && -n "$ROOT_GUID" && -n "$ROOT_TYPE" ]] || { echo "cannot read the root partition layout" >&2; exit 1; }
guestfish -a "$WORK/work.qcow2" <<GF
run
part-expand-gpt /dev/sda
part-del /dev/sda 1
part-add /dev/sda p $ROOT_START -34
part-set-gpt-type /dev/sda 1 $ROOT_TYPE
part-set-gpt-guid /dev/sda 1 $ROOT_GUID
part-set-name /dev/sda 1 $ROOT_NAME
e2fsck-f /dev/sda1
resize2fs /dev/sda1
GF

# Claude Code version to install: newest by default.
CLAUDE_CODE_VERSION="${CLAUDE_CODE_VERSION:-latest}"
[[ "$CLAUDE_CODE_VERSION" =~ ^(latest|stable|next|[0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "CLAUDE_CODE_VERSION must be latest, stable, next or x.y.z" >&2; exit 1; }
echo "==> Claude Code: @anthropic-ai/claude-code@$CLAUDE_CODE_VERSION"
CCSTATUSLINE_VERSION="${CCSTATUSLINE_VERSION:-latest}"
[[ "$CCSTATUSLINE_VERSION" =~ ^(latest|[0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "CCSTATUSLINE_VERSION must be latest or x.y.z" >&2; exit 1; }
echo "==> ccstatusline: ccstatusline@$CCSTATUSLINE_VERSION"
# Status line setup, laid out as it will appear in a home directory. /etc/skel seeds the agent user (created by cloud-init
# on first boot, after this build); /root covers the root shell.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SL="$WORK/statusline"; mkdir -p "$SL/.claude" "$SL/.config/ccstatusline"
cp "$HERE/ccstatusline/claude-settings.json" "$SL/.claude/settings.json"
cp "$HERE/ccstatusline/settings.json" "$SL/.config/ccstatusline/settings.json"

echo "==> customizing image (profile: $PROFILE)"
# The build guest needs a DHCP client inside libguestfs's appliance (isc-dhcp-client on Debian/Ubuntu hosts); without it
# eth0 stays down and every apt call fails with "Temporary failure resolving". Fail early with a clear message instead.
command -v dhclient >/dev/null || { echo "missing dhclient: apt install isc-dhcp-client (libguestfs needs it for guest networking)" >&2; exit 1; }
ARGS=(-a "$WORK/work.qcow2")
if [ -n "$APT_BASE" ]; then
  # point the guest's APT at the mirror for the build (the original file is put back at the end)
  ARGS+=(--run-command "cd /etc/apt/sources.list.d && cp -a ubuntu.sources ubuntu.sources.glasshouse-orig && sed -i -E -e 's#https?://(archive|security)\.ubuntu\.com/ubuntu/?#$APT_BASE/#g' -e 's#https?://ports\.ubuntu\.com/ubuntu-ports/?#$APT_BASE-ports/#g' ubuntu.sources && grep -q '$APT_BASE' ubuntu.sources")
fi
ARGS+=(--update --install "$PKGS")
[ "$PROFILE" = desktop ] && ARGS+=(--install "$DESKTOP_PKGS")
ARGS+=(
  --copy-in "$WORK/99-glasshouse.cfg:/etc/cloud/cloud.cfg.d"
  # Order matters: Claude Code goes in first. It is what CC-Monitor watches (hooks in ~/.claude/settings.json) and
  # CC-Monitor's web UI calls `claude --version`; CC-Monitor is then installed on top of it.
  # Claude Code: needs Node >= 22 for its installer. Installed system-wide as root, so the unprivileged agent user cannot alter it.
  --run-command "node -e 'process.exit(+process.versions.node.split(\".\")[0] >= 22 ? 0 : 1)' || { echo \"nodejs \$(node --version) in this image is older than 22 (needed by Claude Code)\" >&2; exit 1; }"
  --run-command "npm install -g --no-audit --no-fund $NPM_ARG @anthropic-ai/claude-code@$CLAUDE_CODE_VERSION"
  # virt-customize runs commands with PATH=/sbin:/usr/sbin:/bin:/usr/bin, and npm -g installs into /usr/local/bin.
  --run-command "export PATH=/usr/local/sbin:/usr/local/bin:\$PATH; v=\$(claude --version) && echo \"\$v\" > /etc/glasshouse-claude-code-version && cat /etc/glasshouse-claude-code-version"
  # ccstatusline, then its configuration (Claude Code's statusLine command + ccstatusline's layout)
  --run-command "npm install -g --no-audit --no-fund $NPM_ARG ccstatusline@$CCSTATUSLINE_VERSION && export PATH=/usr/local/bin:\$PATH && command -v ccstatusline"
  --copy-in "$SL/.claude:/etc/skel" --copy-in "$SL/.config:/etc/skel"
  --copy-in "$SL/.claude:/root" --copy-in "$SL/.config:/root"
  --run-command "chmod -R a+rX /etc/skel/.claude /etc/skel/.config && chown -R root:root /root/.claude /root/.config && chmod 600 /root/.claude/settings.json"
  # CC-Monitor (pinned commit):
  --copy-in "$WORK/cc-monitor:/opt"
  --copy-in "$WORK/glasshouse-cc-monitor.service:/etc/systemd/system"
  --run-command "chown -R root:root /opt/cc-monitor && chmod -R go-w /opt/cc-monitor"
  # Dependency install for the web UI; verify these steps against upstream's README for your pinned commit.
  # Upstream keeps package.json in webui/; start.sh would otherwise try `npm install` at runtime (no network, /opt not writable).
  --run-command "${CC_MONITOR_SETUP_CMD:-cd /opt/cc-monitor/webui && npm ci --omit=dev $NPM_ARG && test -d node_modules && test -x /opt/cc-monitor/start.sh}"
  --run-command "chown -R root:root /opt/cc-monitor && chmod -R a+rX /opt/cc-monitor"
  # Hook Claude Code as part of installing CC-Monitor. Claude Code must already be there (its version file is written above).
  # The hooks go into the system-wide managed settings, because the agent user does not exist in the image yet (cloud-init
  # creates it at first boot) and because that file is root-owned, so the agent cannot edit it. A session started with
  # monitor.cc_monitor=false removes the file again (see internal/cloudinit).
  --run-command "test -s /etc/glasshouse-claude-code-version && mkdir -p /etc/claude-code && python3 /opt/cc-monitor/install.py --target /etc/claude-code/managed-settings.json --skip-statusline && chown root:root /etc/claude-code/managed-settings.json && chmod 0644 /etc/claude-code/managed-settings.json && grep -q CC-Monitor-hook /etc/claude-code/managed-settings.json"
  --run-command "systemctl mask apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service"
  --run-command "systemctl mask systemd-networkd-wait-online.service"
  --run-command "systemctl disable ssh.socket || true; systemctl enable ssh.service || true"
  --run-command "cloud-init clean --logs --seed || true"
)
if [ -n "$APT_BASE" ] && [ "$KEEP_MIRROR" != 1 ]; then
  ARGS+=(--run-command "mv -f /etc/apt/sources.list.d/ubuntu.sources.glasshouse-orig /etc/apt/sources.list.d/ubuntu.sources")
fi
if [ "$PROFILE" = desktop ]; then
  ARGS+=(--run-command "systemctl set-default graphical.target" --run-command "apt-get purge -y gnome-initial-setup || true" --run-command "systemctl enable gdm3")
else
  ARGS+=(--run-command "systemctl set-default multi-user.target")
fi
# libguestfs forwards the host's proxy env into the appliance, where 127.0.0.1 is the guest itself: drop it here.
env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY -u all_proxy -u ALL_PROXY -u no_proxy -u NO_PROXY \
  virt-customize "${ARGS[@]}"
virt-sysprep -a "$WORK/work.qcow2" --operations defaults,-user-account,-customize

echo "==> writing image"
OUT="$OUT_DIR/ubuntu-26.04-$ARCH-$PROFILE.$FORMAT"
# Write next to the target and rename into place: a session that still has the old image open (qemu holds a write lock
# on it) does not block the build, it keeps its open file, and a failed write never leaves a half-written image behind.
TMP_OUT="$OUT.new.$$"
trap 'rm -rf "$WORK" "$TMP_OUT" "$TMP_OUT.sha256"' EXIT
if [ "$FORMAT" = raw ]; then qemu-img convert -O raw "$WORK/work.qcow2" "$TMP_OUT"
else qemu-img convert -O qcow2 -c "$WORK/work.qcow2" "$TMP_OUT"; fi
chmod 600 "$TMP_OUT"
echo "$(sha256sum "$TMP_OUT" | cut -d' ' -f1)  $(basename "$OUT")" > "$TMP_OUT.sha256"
chmod 644 "$TMP_OUT.sha256"
mv -f "$TMP_OUT.sha256" "$OUT.sha256"
mv -f "$TMP_OUT" "$OUT"
echo "done: $OUT"
echo "      $(cut -d' ' -f1 "$OUT.sha256")"
