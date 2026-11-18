#!/bin/bash
# Runs as root INSIDE the arm64 VM started by image/build-mac.sh (from cloud-init's runcmd).
#
#   provision.sh          build: does what build.sh does from the outside with virt-customize + virt-sysprep (keep the
#                         two in step), checks the result and cleans the guest. Last line: GLASSHOUSE-BUILD-OK|FAILED.
#   provision.sh verify   second, network-less boot of the finished image from a fresh EFI variable store: proves the
#                         image boots, checks every packaged file and the EFI boot files, cleans the guest again.
#                         Last line: GLASSHOUSE-VERIFY-OK|FAILED.
# Progress goes to the virtio console (/dev/hvc0), which build-mac.sh shows on the Mac; the VM powers off either way.
#
# Disk corruption on the Mac: images came out with zeroed kernels, shims and libraries (identical size and metadata,
# whole 4 KiB pages of zeros), which hang at boot or crash with SIGILL. The cause was the helper attaching the sparse raw
# disk with cachingMode .automatic, which can return stale zeros to the guest under memory pressure; the helper now
# uses .cached (see vz-helper main.swift). Discard stays off on every virtual disk as well (in the build and in the
# image): it only punches more holes into the sparse file, and the image stays the same either way. The verify boot
# below catches any corruption that still gets through.
set -euo pipefail

MODE="${1:-build}"
case "$MODE" in build) TAG=BUILD;; verify) TAG=VERIFY;; *) echo "unknown mode $MODE" >&2; exit 2;; esac

D="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CON=/dev/hvc0; [ -w "$CON" ] || CON=/dev/console
LOG=/var/log/glasshouse-$MODE.log
# A getty on the console hangs the terminal up when it starts (vhangup), which kills anything writing to it. So the
# script itself writes only to a log file, and a separate, expendable process copies that log to the console. The
# getty is also masked for this boot only (--runtime: nothing of it ends up in the image).
systemctl mask --runtime --now serial-getty@hvc0.service >/dev/null 2>&1 || true
: >> "$LOG"
( tail -n +1 -F "$LOG" > "$CON" 2>/dev/null ) &
FORWARDER=$!
exec >> "$LOG" 2>&1 < /dev/null
trap '' PIPE HUP

finish() {
  rc=$?
  if [ "$rc" -eq 0 ]; then msg="GLASSHOUSE-$TAG-OK"; else msg="GLASSHOUSE-$TAG-FAILED rc=$rc (see the lines above)"; fi
  echo "$msg"
  sleep 2; kill "$FORWARDER" 2>/dev/null || true
  # the marker the host waits for, written straight to the console (retried in case the console is being reset)
  for _ in 1 2 3 4 5; do echo "$msg" > "$CON" 2>/dev/null && break; sleep 1; done
  sync; sleep 2
  systemctl poweroff --no-block || poweroff -f
}
trap finish EXIT

# shellcheck source=/dev/null
. "$D/env.sh"
export DEBIAN_FRONTEND=noninteractive TERM=dumb
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
NPM_ARG=""; [ -z "$NPM_REGISTRY" ] || NPM_ARG="--registry=$NPM_REGISTRY"
APT=(apt-get -y -q -o Dpkg::Progress-Fancy=0 -o APT::Color=0 -o DPkg::Lock::Timeout=600 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold)
step() { echo "==> [guest] $*"; }

# Refuse discards at the block layer right now (the build's cloud-init bootcmd already did this early in the boot).
no_discard_now() {
  local q
  for q in /sys/block/vd*/queue/discard_max_bytes; do [ -w "$q" ] && echo 0 > "$q"; done
  grep -H . /sys/block/vd*/queue/discard_max_bytes | sed 's#^#    #'
}

# Every packaged file must match its dpkg checksum (modified conffiles are expected), and the EFI boot files must match
# the copies their packages ship. Read from disk, not from the page cache.
check_integrity() {
  step "checking file integrity"
  sync; echo 3 > /proc/sys/vm/drop_caches
  local bad
  bad="$(dpkg --verify 2>/dev/null | awk '$2 != "c" && substr($1,3,1) == "5"' || true)"
  if [ -n "$bad" ]; then
    echo "packaged files with a wrong checksum (corrupted disk?):"; echo "$bad" | head -40
    return 1
  fi
  echo "    dpkg --verify: all packaged files match"
  local f ok cand
  for f in /boot/efi/EFI/BOOT/BOOTAA64.EFI /boot/efi/EFI/ubuntu/shimaa64.efi /boot/efi/EFI/ubuntu/mmaa64.efi \
           /boot/efi/EFI/BOOT/mmaa64.efi /boot/efi/EFI/BOOT/fbaa64.efi /boot/efi/EFI/ubuntu/grubaa64.efi; do
    [ -f "$f" ] || continue
    ok=0
    for cand in /usr/lib/shim/*.efi* /usr/lib/grub/arm64-efi-signed/*.efi*; do
      [ -f "$cand" ] && cmp -s "$f" "$cand" && { ok=1; break; }
    done
    if [ "$ok" = 1 ]; then echo "    $f: matches its package"; continue; fi
    # not byte-identical to an installed package copy (a different shim/grub version): at least refuse a zeroed tail
    if python3 - "$f" <<'PY'
import sys
d = open(sys.argv[1], "rb").read()
run = best = 0
for b in d:
    run = run + 1 if b == 0 else 0
    best = max(best, run)
sys.exit(1 if best >= 256 * 1024 or d[-4096:] == bytes(4096) else 0)
PY
    then echo "    $f: no package copy matches (version drift?); no zeroed blocks found"
    else echo "    $f: CORRUPTED (long run of zero bytes)"; return 1
    fi
  done
}

# What virt-sysprep does on Linux, so every session starts as a fresh instance.
clean_guest() {
  step "cleaning up"
  "${APT[@]}" autoremove --purge || true
  apt-get clean
  rm -rf /root/.npm /root/.cache /root/.bash_history /var/tmp/* /var/lib/dhcp/* /var/lib/systemd/random-seed
  rm -f /etc/ssh/ssh_host_* /var/lib/dbus/machine-id
  rm -f /etc/netplan/50-cloud-init.yaml   # the build VM's NAT NIC; sessions on the Mac have no NIC
  cloud-init clean --logs --seed --machine-id 2>/dev/null || { cloud-init clean --logs --seed || true; truncate -s 0 /etc/machine-id; }
  journalctl --rotate >/dev/null 2>&1 || true; journalctl --vacuum-time=1s >/dev/null 2>&1 || true
  find /var/log -type f \( -name '*.gz' -o -name '*.[0-9]' \) -delete 2>/dev/null || true
  cd /
  rm -rf /root/ghbuild
  sync
  # deliberately no fstrim: discard is off, see the note at the top
}

step "$MODE: $(uname -m), $(. /etc/os-release; echo "$PRETTY_NAME"), kernel $(uname -r), profile $PROFILE"
[ "$(uname -m)" = aarch64 ] || { echo "expected an aarch64 guest" >&2; exit 1; }
no_discard_now

if [ "$MODE" = verify ]; then
  # this boot got here, so firmware, shim, grub, kernel and initrd of the finished image work from a fresh NVRAM
  step "the image boots (fresh EFI variable store, no network)"
  check_integrity
  clean_guest
  step "done"
  exit 0
fi

# first-boot apt jobs would hold the dpkg lock for minutes
systemctl stop apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service 2>/dev/null || true

if [ -n "$APT_BASE" ]; then
  step "APT mirror $APT_BASE"
  cd /etc/apt/sources.list.d
  cp -a ubuntu.sources ubuntu.sources.glasshouse-orig
  # older arm64 releases use ports.ubuntu.com/ubuntu-ports, which mirrors publish as <base>-ports
  sed -i -E -e "s#https?://ports\.ubuntu\.com/ubuntu-ports/?#$APT_BASE-ports/#g" \
            -e "s#https?://(archive|security)\.ubuntu\.com/ubuntu/?#$APT_BASE/#g" ubuntu.sources
  grep -q "$APT_BASE" ubuntu.sources
  cd /
fi

step "installing packages"
"${APT[@]}" update
"${APT[@]}" install git build-essential python3 python3-pip python3-venv nodejs npm curl ca-certificates \
  openssh-server socat jq ripgrep unzip sudo
if [ "$PROFILE" = desktop ]; then
  step "installing the GNOME desktop"
  "${APT[@]}" install ubuntu-desktop-minimal gdm3 gnome-terminal libgl1-mesa-dri fonts-noto-cjk
fi
install -m 0644 "$D/99-glasshouse.cfg" /etc/cloud/cloud.cfg.d/99-glasshouse.cfg

step "Claude Code @anthropic-ai/claude-code@$CLAUDE_CODE_VERSION"
node -e 'process.exit(+process.versions.node.split(".")[0] >= 22 ? 0 : 1)' \
  || { echo "nodejs $(node --version) in this image is older than 22 (needed by Claude Code)" >&2; exit 1; }
npm install -g --no-audit --no-fund $NPM_ARG "@anthropic-ai/claude-code@$CLAUDE_CODE_VERSION"
claude --version > /etc/glasshouse-claude-code-version
cat /etc/glasshouse-claude-code-version

step "ccstatusline@$CCSTATUSLINE_VERSION"
npm install -g --no-audit --no-fund $NPM_ARG "ccstatusline@$CCSTATUSLINE_VERSION"
command -v ccstatusline
for home in /etc/skel /root; do
  cp -a "$D/statusline/.claude" "$D/statusline/.config" "$home/"
done
chmod -R a+rX /etc/skel/.claude /etc/skel/.config
chown -R root:root /root/.claude /root/.config /etc/skel/.claude /etc/skel/.config
chmod 600 /root/.claude/settings.json

step "CC-Monitor"
rm -rf /opt/cc-monitor
cp -a "$D/cc-monitor" /opt/cc-monitor
install -m 0644 "$D/glasshouse-cc-monitor.service" /etc/systemd/system/glasshouse-cc-monitor.service
chown -R root:root /opt/cc-monitor && chmod -R go-w /opt/cc-monitor
bash -c "${CC_MONITOR_SETUP_CMD:-cd /opt/cc-monitor/webui && npm ci --omit=dev $NPM_ARG && test -d node_modules && test -x /opt/cc-monitor/start.sh}"
chown -R root:root /opt/cc-monitor && chmod -R a+rX /opt/cc-monitor
test -s /etc/glasshouse-claude-code-version
mkdir -p /etc/claude-code
python3 /opt/cc-monitor/install.py --target /etc/claude-code/managed-settings.json --skip-statusline
chown root:root /etc/claude-code/managed-settings.json && chmod 0644 /etc/claude-code/managed-settings.json
grep -q CC-Monitor-hook /etc/claude-code/managed-settings.json

step "services"
systemctl mask apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service
systemctl mask systemd-networkd-wait-online.service
systemctl disable ssh.socket || true; systemctl enable ssh.service || true
if [ "$PROFILE" = desktop ]; then
  systemctl set-default graphical.target
  "${APT[@]}" purge gnome-initial-setup || true
  systemctl enable gdm3
else
  systemctl set-default multi-user.target
fi
if [ -n "$APT_BASE" ] && [ "$KEEP_MIRROR" != 1 ]; then
  mv -f /etc/apt/sources.list.d/ubuntu.sources.glasshouse-orig /etc/apt/sources.list.d/ubuntu.sources
fi

step "no discard on virtual disks (sessions too)"
# block layer: every virtio disk refuses discard from the moment udev sees it
cat > /etc/udev/rules.d/60-glasshouse-no-discard.rules <<'RULE'
# Glasshouse: no discard on the sparse VZ raw disks (it only punches holes; see image/provision-guest.sh).
ACTION=="add|change", SUBSYSTEM=="block", KERNEL=="vd[a-z]", ATTR{queue/discard_max_bytes}="0"
RULE
# filesystems: no online discard, no periodic fstrim
sed -i -E 's/(^[^#].*[[:space:]])discard,/\1/; s/,discard([,[:space:]])/\1/; s/([[:space:]])discard([[:space:]])/\1defaults\2/' /etc/fstab
grep -q discard /etc/fstab && { echo "fstab still mentions discard:"; cat /etc/fstab; exit 1; }
cat /etc/fstab | sed 's#^#    #'
systemctl mask fstrim.timer fstrim.service

check_integrity
clean_guest
step "done"
