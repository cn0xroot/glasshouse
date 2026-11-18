#!/usr/bin/env bash
# Build a Glasshouse base image ON A MAC with Apple silicon, from Canonical's official Ubuntu 26.04 LTS **arm64** cloud
# image, using Virtualization.framework. The result is the raw image the macOS backend boots:
#   $GLASSHOUSE_HOME/images/ubuntu-26.04-arm64-<profile>.raw (+ .sha256)
#
#   CC_MONITOR_REF=<40-hex commit> ./image/build-mac.sh [--profile headless|desktop] [--mirror auto|official|tuna|ustc]
#                                                       [--keep-mirror] [--cpus N] [--memory MiB]
#
# How it differs from image/build.sh (Linux): there is no libguestfs on macOS, so instead of editing the disk from the
# outside this script boots the arm64 cloud image once in a build VM (the glasshouse-vz helper with a NAT network device,
# which only image builds get), lets cloud-init run the same customization inside the guest, cleans the guest up and
# powers it off. Sources, verification and installed software are the same as in build.sh.
#
# Why the cloud image and not the live-server ISO: Glasshouse configures every session with cloud-init, and the cloud
# image is exactly an installed Ubuntu with cloud-init, signed by Canonical. The ISO is an installer; it would need an
# unattended autoinstall run to end up at the same place.
#
# Needs: macOS 13+ on Apple silicon, Xcode command line tools (swift, for the helper), and from Homebrew:
#   brew install qemu gnupg        # qemu-img converts the qcow2 cloud image to raw; gpg/gpgv verify Canonical's signature
# Optional environment: APT_MIRROR, NPM_REGISTRY, CC_MONITOR_REPO, CLAUDE_CODE_VERSION, CCSTATUSLINE_VERSION,
# CC_MONITOR_SETUP_CMD (all as in build.sh), plus:
#   UBUNTU_KEYRING=<file>      an existing ubuntu-cloudimage-keyring.gpg instead of fetching the key from keyserver.ubuntu.com
#   BUILD_TIMEOUT=<seconds>    give up on the build VM after this long (default 5400)
#   GLASSHOUSE_VZ=<path>       the glasshouse-vz helper (default: bin/glasshouse-vz, built with `make vz-helper` if missing)
set -euo pipefail

ARCH=arm64
PROFILE=desktop
MIRROR=""
KEEP_MIRROR=0
CPUS=""
MEM=""
while [ $# -gt 0 ]; do
  case "$1" in
    --profile) PROFILE="$2"; shift 2;;
    --mirror) MIRROR="$2"; shift 2;;
    --keep-mirror) KEEP_MIRROR=1; shift;;
    --cpus) CPUS="$2"; shift 2;;
    --memory) MEM="$2"; shift 2;;
    -h|--help) awk 'NR>1 && /^set -euo/{exit} NR>1{print}' "$0"; exit 0;;
    *) echo "unknown option $1" >&2; exit 2;;
  esac
done
case "$PROFILE" in headless|desktop) ;; *) echo "bad --profile" >&2; exit 2;; esac

[ "$(uname -s)" = Darwin ] || { echo "build-mac.sh runs on macOS; on Linux use image/build.sh" >&2; exit 1; }
[ "$(uname -m)" = arm64 ] || { echo "build-mac.sh needs Apple silicon (arm64); under Rosetta run: arch -arm64 $0" >&2; exit 1; }
[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 0 ] || { echo "this shell runs under Rosetta; use a native arm64 terminal" >&2; exit 1; }

: "${CC_MONITOR_REF:?set CC_MONITOR_REF to a 40-hex commit of github.com/cn0xroot/CC-Monitor (review it first)}"
[[ "$CC_MONITOR_REF" =~ ^[0-9a-f]{40}$ ]] || { echo "CC_MONITOR_REF must be a full 40-character commit hash" >&2; exit 1; }

for t in qemu-img gpg gpgv git curl shasum hdiutil base64; do
  command -v "$t" >/dev/null || { echo "missing tool: $t (brew install qemu gnupg)" >&2; exit 1; }
done

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
VZ="${GLASSHOUSE_VZ:-$ROOT/bin/glasshouse-vz}"
if [ ! -x "$VZ" ] || ! "$VZ" check >/dev/null 2>&1; then
  [ -z "${GLASSHOUSE_VZ:-}" ] || { echo "$VZ does not run; rebuild it with: make vz-helper" >&2; exit 1; }
  echo "==> building the Virtualization.framework helper"
  make -C "$ROOT" vz-helper >/dev/null
fi
"$VZ" check | grep -q '"supported":true' || { echo "Virtualization.framework is not available on this Mac" >&2; exit 1; }

HOME_DIR="${GLASSHOUSE_HOME:-$HOME/.local/share/glasshouse}"
OUT_DIR="$HOME_DIR/images"; CACHE="$HOME_DIR/cache"
mkdir -p "$OUT_DIR" "$CACHE"; chmod 700 "$HOME_DIR"
# The work directory holds the (sparse) raw disk, so it must be on the same APFS volume as the images directory; it also
# holds the helper's unix sockets, whose paths must stay under 104 bytes.
WORK="$(mktemp -d "$HOME_DIR/build.XXXXXX")"
HELPER_PID=""; TAIL_PID=""
cleanup() {
  [ -z "$HELPER_PID" ] || kill "$HELPER_PID" 2>/dev/null || true
  [ -z "$TAIL_PID" ] || kill "$TAIL_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT
[ ${#WORK} -le 85 ] || { echo "work directory path too long for unix sockets: $WORK (set GLASSHOUSE_HOME to a shorter path)" >&2; exit 1; }

# ---- options shared with build.sh ------------------------------------------------------------------------------
MIRROR="${MIRROR:-${GLASSHOUSE_MIRROR:-auto}}"
case "$MIRROR" in auto|official|tuna|ustc) ;; *) echo "bad --mirror (auto|official|tuna|ustc)" >&2; exit 2;; esac
URL_RE='^https?://[A-Za-z0-9._:/-]+$'
[[ -z "${APT_MIRROR:-}" || "$APT_MIRROR" =~ $URL_RE ]] || { echo "APT_MIRROR must be an http(s) URL" >&2; exit 1; }
NPM_REGISTRY="${NPM_REGISTRY:-}"
[[ -z "$NPM_REGISTRY" || "$NPM_REGISTRY" =~ $URL_RE ]] || { echo "NPM_REGISTRY must be an http(s) URL" >&2; exit 1; }
CC_MONITOR_REPO="${CC_MONITOR_REPO:-https://github.com/cn0xroot/CC-Monitor.git}"
[[ "$CC_MONITOR_REPO" =~ ^https://[A-Za-z0-9._:/@~+-]+$ ]] || { echo "CC_MONITOR_REPO must be an https git URL" >&2; exit 1; }
CLAUDE_CODE_VERSION="${CLAUDE_CODE_VERSION:-latest}"
[[ "$CLAUDE_CODE_VERSION" =~ ^(latest|stable|next|[0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "CLAUDE_CODE_VERSION must be latest, stable, next or x.y.z" >&2; exit 1; }
CCSTATUSLINE_VERSION="${CCSTATUSLINE_VERSION:-latest}"
[[ "$CCSTATUSLINE_VERSION" =~ ^(latest|[0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "CCSTATUSLINE_VERSION must be latest or x.y.z" >&2; exit 1; }
BUILD_TIMEOUT="${BUILD_TIMEOUT:-5400}"
[[ "$BUILD_TIMEOUT" =~ ^[0-9]+$ ]] || { echo "BUILD_TIMEOUT must be a number of seconds" >&2; exit 1; }

if [ "$PROFILE" = desktop ]; then BUILD_GB=20; DEF_MEM=6144; else BUILD_GB=8; DEF_MEM=4096; fi
CPUS="${CPUS:-4}"; MEM="${MEM:-$DEF_MEM}"
[[ "$CPUS" =~ ^[0-9]+$ && "$MEM" =~ ^[0-9]+$ ]] || { echo "--cpus and --memory must be numbers" >&2; exit 2; }

# ---- Canonical's signing key -----------------------------------------------------------------------------------
# "UEC Image Automatic Signing Key <cdimage@ubuntu.com>", the key that signs cloud-images.ubuntu.com's SHA256SUMS
# (the same key as in Ubuntu's ubuntu-cloudimage-keyring package). Fetched by full fingerprint and checked again.
UBUNTU_CLOUD_KEY=D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81
KEYRING="${UBUNTU_KEYRING:-$CACHE/ubuntu-cloudimage-keyring.gpg}"
if [ ! -s "$KEYRING" ]; then
  [ -z "${UBUNTU_KEYRING:-}" ] || { echo "UBUNTU_KEYRING=$UBUNTU_KEYRING does not exist" >&2; exit 1; }
  echo "==> fetching Ubuntu's cloud image signing key ($UBUNTU_CLOUD_KEY)"
  # a short GNUPGHOME: gpg talks to dirmngr over a unix socket inside it, and long paths fail ("File name too long")
  GH_GNUPG="$(mktemp -d /tmp/ghgpg.XXXXXX)"
  GNUPGHOME="$GH_GNUPG" gpg --batch --quiet --keyserver hkps://keyserver.ubuntu.com --recv-keys "$UBUNTU_CLOUD_KEY" \
    && GNUPGHOME="$GH_GNUPG" gpg --batch --export "$UBUNTU_CLOUD_KEY" > "$KEYRING.new" || rc=$?
  GNUPGHOME="$GH_GNUPG" gpgconf --kill all 2>/dev/null || true
  rm -rf "$GH_GNUPG"
  [ "${rc:-0}" = 0 ] && [ -s "$KEYRING.new" ] || { rm -f "$KEYRING.new"; echo "cannot fetch key $UBUNTU_CLOUD_KEY from keyserver.ubuntu.com (or set UBUNTU_KEYRING)" >&2; exit 1; }
  mv "$KEYRING.new" "$KEYRING"
fi
gpg --batch --with-colons --show-keys "$KEYRING" 2>/dev/null | grep -q "^fpr:*$UBUNTU_CLOUD_KEY:" \
  || { echo "$KEYRING does not hold key $UBUNTU_CLOUD_KEY; delete it and run again" >&2; rm -f "$CACHE/ubuntu-cloudimage-keyring.gpg"; exit 1; }

# ---- download the arm64 cloud image (same sources as build.sh) -------------------------------------------------
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
probe_ms() {
  local t
  t="$(curl -fsSL -r 0-524287 -o /dev/null -m 8 -w '%{time_total}' "$(src_dir "$1")/$(src_img "$1")" 2>/dev/null)" || { echo 99999; return; }
  awk -v t="$t" 'BEGIN{printf "%d", t*1000}'
}

case "$MIRROR" in
  official) CANDIDATES="official";;
  tuna|ustc) CANDIDATES="$MIRROR official";;
  auto)
    for s in official tuna ustc; do ( probe_ms "$s" > "$WORK/probe.$s" ) & done; wait
    MS_OFF="$(cat "$WORK/probe.official")"; MS_TUNA="$(cat "$WORK/probe.tuna")"; MS_USTC="$(cat "$WORK/probe.ustc")"
    best=tuna; MS_BEST=$MS_TUNA; [ "$MS_USTC" -lt "$MS_TUNA" ] && { best=ustc; MS_BEST=$MS_USTC; }
    pick=official
    if [ "$MS_OFF" -ge 99999 ] || [ $(( MS_BEST * 2 )) -lt "$MS_OFF" ]; then pick="$best"; fi
    [ "$pick" = official ] || [ "$MS_BEST" -lt 99999 ] || pick=official
    echo "==> mirror: auto -> $pick (official $MS_OFF ms, tuna $MS_TUNA ms, ustc $MS_USTC ms; 99999 = unreachable)"
    CANDIDATES="$pick"; for s in official tuna ustc; do [ "$s" = "$pick" ] || CANDIDATES="$CANDIDATES $s"; done;;
esac

IMG=""; APT_SRC=official
fetch_image() {
  local s dir img
  for s in $CANDIDATES; do
    dir="$(src_dir "$s")"; img="$(src_img "$s")"
    echo "==> downloading and verifying $img (source: $s)"
    if ! { curl -fsSL -m 60 -o "$WORK/SHA256SUMS" "$dir/SHA256SUMS" && curl -fsSL -m 60 -o "$WORK/SHA256SUMS.gpg" "$dir/SHA256SUMS.gpg"; }; then
      echo "    $s: cannot fetch the checksum list; trying the next source" >&2; continue
    fi
    gpgv --keyring "$KEYRING" "$WORK/SHA256SUMS.gpg" "$WORK/SHA256SUMS" \
      || { echo "    $s: BAD SIGNATURE on SHA256SUMS; not using this source" >&2; continue; }
    grep " \*\{0,1\}$img\$" "$WORK/SHA256SUMS" > "$WORK/SUM" \
      || { echo "    $s: $img is not in the signed list; trying the next source" >&2; continue; }
    if [ -f "$CACHE/$img" ] && ( cd "$CACHE" && shasum -a 256 -c "$WORK/SUM" >/dev/null 2>&1 ); then
      echo "    cached copy is up to date"; IMG="$img"; APT_SRC="$s"; return 0
    fi
    # resume across dropped connections (curl 56 "unexpected eof" is common on long downloads); the checksum below
    # catches anything a resume could get wrong. The .part file is per source, so sources never mix bytes.
    local ok=0 try
    for try in 1 2 3 4 5; do
      if curl -fL --retry 3 --retry-all-errors -C - -o "$CACHE/$img.part" "$dir/$img"; then ok=1; break; fi
      echo "    $s: download interrupted (attempt $try); resuming" >&2; sleep 3
    done
    if [ "$ok" != 1 ]; then
      rm -f "$CACHE/$img.part"; echo "    $s: download failed; trying the next source" >&2; continue
    fi
    mv "$CACHE/$img.part" "$CACHE/$img"
    if ! ( cd "$CACHE" && shasum -a 256 -c "$WORK/SUM" ); then
      rm -f "$CACHE/$img"; echo "    $s: checksum mismatch; trying the next source" >&2; continue
    fi
    IMG="$img"; APT_SRC="$s"; return 0
  done
  return 1
}
fetch_image || { echo "no source could provide a verified $ARCH cloud image" >&2; exit 1; }
APT_BASE="${APT_MIRROR:-$(src_apt "$APT_SRC")}"; APT_BASE="${APT_BASE%/}"
[ -z "$APT_BASE" ] || echo "==> APT source for the build guest: $APT_BASE"

# ---- build inputs ----------------------------------------------------------------------------------------------
IN="$WORK/in/glasshouse-build"; mkdir -p "$IN"
echo "==> fetching CC-Monitor @ $CC_MONITOR_REF"
git clone --quiet "$CC_MONITOR_REPO" "$IN/cc-monitor"
git -C "$IN/cc-monitor" checkout --quiet --detach "$CC_MONITOR_REF"
[ "$(git -C "$IN/cc-monitor" rev-parse HEAD)" = "$CC_MONITOR_REF" ]
rm -rf "$IN/cc-monitor/.git"

mkdir -p "$IN/statusline/.claude" "$IN/statusline/.config/ccstatusline"
cp "$HERE/ccstatusline/claude-settings.json" "$IN/statusline/.claude/settings.json"
cp "$HERE/ccstatusline/settings.json" "$IN/statusline/.config/ccstatusline/settings.json"

cat > "$IN/glasshouse-cc-monitor.service" <<'UNIT'
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
printf 'datasource_list: [NoCloud, None]\n' > "$IN/99-glasshouse.cfg"

{
  printf 'PROFILE=%q\n' "$PROFILE"
  printf 'APT_BASE=%q\n' "$APT_BASE"
  printf 'KEEP_MIRROR=%q\n' "$KEEP_MIRROR"
  printf 'NPM_REGISTRY=%q\n' "$NPM_REGISTRY"
  printf 'CLAUDE_CODE_VERSION=%q\n' "$CLAUDE_CODE_VERSION"
  printf 'CCSTATUSLINE_VERSION=%q\n' "$CCSTATUSLINE_VERSION"
  printf 'CC_MONITOR_SETUP_CMD=%q\n' "${CC_MONITOR_SETUP_CMD:-}"
} > "$IN/env.sh"
cp "$HERE/provision-guest.sh" "$IN/provision.sh"
echo glasshouse-build > "$IN/MANIFEST"
# A plain tar file is a valid read-only disk (its size is a multiple of 512 bytes) and keeps the exec bits that an
# ISO without Rock Ridge would drop. The guest finds it by its MANIFEST entry.
COPYFILE_DISABLE=1 tar --format ustar -cf "$WORK/inputs.tar" -C "$WORK/in" glasshouse-build 2>/dev/null \
  || COPYFILE_DISABLE=1 tar -cf "$WORK/inputs.tar" -C "$WORK/in" glasshouse-build

# ---- cloud-init seeds: one per boot (build, then verify); each runs provision.sh from the inputs disk once ------
# make_seed <dir> <mode> <instance-id prefix>
make_seed() {
  mkdir -p "$1"
  cat > "$1/meta-data" <<EOF
instance-id: $3-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
local-hostname: glasshouse-build
EOF
  cat > "$1/user-data" <<EOF
#cloud-config
users: []
disable_root: true
ssh_pwauth: false
growpart: {mode: auto, devices: ['/']}
resize_rootfs: true
bootcmd:
  # early: no discard on the virtual disks (it only punches holes into the sparse raw file; see provision-guest.sh)
  - [ sh, -c, 'for q in /sys/block/vd*/queue/discard_max_bytes; do echo 0 > "\$q"; done; true' ]
  # no login prompt on the console: its getty would hang up the terminal the progress goes to
  - [ sh, -c, 'systemctl mask --runtime --now serial-getty@hvc0.service >/dev/null 2>&1 || true' ]
  - [ sh, -c, 'echo "glasshouse-build: guest is up ($2)" > /dev/hvc0 || true' ]
runcmd:
  - [ sh, -c, 'for d in /dev/vd?; do if tar -tf "\$d" glasshouse-build/MANIFEST >/dev/null 2>&1; then mkdir -p /root/ghbuild && tar -xf "\$d" -C /root/ghbuild && exec bash /root/ghbuild/glasshouse-build/provision.sh $2; fi; done; echo "GLASSHOUSE-FAILED no build inputs" > /dev/hvc0; poweroff' ]
EOF
  hdiutil makehybrid -quiet -iso -joliet -default-volume-name cidata -o "$1.iso" "$1"
}
make_seed "$WORK/seed" build glasshouse-build
make_seed "$WORK/vseed" verify glasshouse-verify

# ---- disk: arm64 qcow2 cloud image -> raw, grown for the package installs (cloud-init grows the partition) -----
echo "==> converting $IMG to a ${BUILD_GB}G raw disk"
qemu-img convert -O raw "$CACHE/$IMG" "$WORK/disk.raw"
qemu-img resize -q -f raw "$WORK/disk.raw" "${BUILD_GB}G"
chmod 600 "$WORK/disk.raw"

# run_vm <seed.iso> <nat true|false> <timeout s> <marker>: boots the disk once and waits for the guest to power off;
# succeeds when the guest printed <marker>-OK. A fresh EFI variable store every time, as every session gets one.
run_vm() {
  local seed="$1" nat="$2" timeout="$3" marker="$4" waited=0
  rm -f "$WORK/efi-vars" "$WORK/vz.ready"
  cat > "$WORK/vz.json" <<EOF
{
  "cpus": $CPUS, "memoryMiB": $MEM,
  "disk": "$WORK/disk.raw", "seed": "$seed", "gui": false,
  "sshSock": "$WORK/ssh.sock", "ctrlSock": "$WORK/ctrl.sock", "efiVars": "$WORK/efi-vars", "readyFile": "$WORK/vz.ready",
  "forwards": [], "nat": $nat, "extraDisks": ["$WORK/inputs.tar"]
}
EOF
  : > "$WORK/console.log"
  "$VZ" run --config "$WORK/vz.json" > "$WORK/vz.log" 2>&1 & HELPER_PID=$!
  tail -n +1 -F "$WORK/console.log" 2>/dev/null & TAIL_PID=$!
  while kill -0 "$HELPER_PID" 2>/dev/null; do
    if [ "$waited" -ge "$timeout" ]; then
      echo "the VM did not finish within ${timeout}s; stopping it" >&2
      # the helper asks the guest to power off and forces it after a grace period: give it that time before SIGKILL
      kill "$HELPER_PID" 2>/dev/null || true
      for _ in $(seq 1 12); do kill -0 "$HELPER_PID" 2>/dev/null || break; sleep 5; done
      kill -9 "$HELPER_PID" 2>/dev/null || true
      break
    fi
    sleep 5; waited=$((waited + 5))
  done
  wait "$HELPER_PID" 2>/dev/null || true; HELPER_PID=""
  sleep 1; kill "$TAIL_PID" 2>/dev/null || true; TAIL_PID=""
  if ! grep -q "$marker-OK" "$WORK/console.log"; then
    echo "image build FAILED ($marker). Helper log:" >&2
    cat "$WORK/vz.log" >&2
    cp "$WORK/console.log" "$HOME_DIR/build-mac-console.log" 2>/dev/null && echo "guest console saved to $HOME_DIR/build-mac-console.log" >&2
    return 1
  fi
}

echo "==> booting the arm64 build VM ($CPUS vCPU, $MEM MiB, profile $PROFILE); guest output follows"
run_vm "$WORK/seed.iso" true "$BUILD_TIMEOUT" GLASSHOUSE-BUILD || exit 1

# Boot the finished image once more the way a session does (no network, fresh EFI variable store) and check every
# packaged file and the EFI boot files from disk. An image that does not boot, or has corrupted files, is not written.
echo "==> verifying the image: second boot, no network"
run_vm "$WORK/vseed.iso" false 900 GLASSHOUSE-VERIFY || { echo "the built image failed verification; nothing written" >&2; exit 1; }

# ---- write the image -------------------------------------------------------------------------------------------
echo "==> writing image"
OUT="$OUT_DIR/ubuntu-26.04-$ARCH-$PROFILE.raw"
echo "$(shasum -a 256 "$WORK/disk.raw" | cut -d' ' -f1)  $(basename "$OUT")" > "$WORK/disk.raw.sha256"
chmod 644 "$WORK/disk.raw.sha256"
# same volume: a rename, so a running session keeps its open (cloned) file and a failure never leaves half an image
mv -f "$WORK/disk.raw.sha256" "$OUT.sha256"
mv -f "$WORK/disk.raw" "$OUT"
echo "done: $OUT"
echo "      $(cut -d' ' -f1 "$OUT.sha256")"
