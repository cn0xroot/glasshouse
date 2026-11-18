# Changelog

English | [简体中文](./CHANGELOG.zh-CN.md)

What changed per version, as a user would notice it. Every item says how far it was checked: **verified** means it was run on a real machine, **not verified** means only the code was written or only unit tests exist. The step-by-step code change log is the local note `docs/CHANGELOG.md` (listed in `.gitignore`, not in the repository).

## V0.2.1 (2026-11-17, tag `V0.2.1`)

On top of `V0.2`; fixes `glasshouse monitor`.

- **Fixed: `glasshouse monitor` timed out when `~/.ssh/config` enables `ControlMaster`.** Every session uses the same host alias `glasshouse` (macOS backend), so ssh reused another session's master and the `monitor.sock` forward never appeared. All ssh calls now carry `-o ControlMaster=no -o ControlPath=none`. (From the Mac-side patch.) On Linux, `ssh -G` showed that the command-line options override `ControlMaster auto` in the config file; a real master collision was not reproduced because Linux sessions have different ports.
- **macOS:** the tunnel's ssh prints a harmless `setsockopt TCP_NODELAY` error for every forwarded connection; it is filtered out on macOS only. **Linux is unchanged** (ssh's stderr goes straight to a file).
- **Fixed: a possible hang on exit.** The patch first turned ssh's stderr into an `io.Pipe`; on exit, `waitCmd`'s goroutine and the cleanup `defer` then both called `Wait` on the tunnel and could block each other forever over the same "copy finished" signal. Reproduced in isolation: 5 hangs in 300 runs with a pipe, 0 with a file. The tunnel is now waited for once, and the result goes to every receiver through a channel that is closed afterwards.

## V0.2 (2026-11-17, tag `V0.2`)

On top of `V0.1`; main commits `3f8429f` and `5f3716d`. Main points: support for Macs with Apple silicon, and a fix for a race that V0.1 already had.

### Added: Apple silicon (macOS) support

- **arm64 image build**, `image/build-mac.sh` and `image/provision-guest.sh`: builds the image on a Mac from Ubuntu 26.04's **arm64** cloud image, inside a Virtualization.framework VM. After the build the image is booted once more without network and checked (`dpkg --verify` plus the EFI boot files); an image that fails is not written. Apple silicon runs arm64 guests only, so the x86 images of V0.1 cannot be used.
- **`network.mode: host` on macOS:** the guest uses the Mac's network directly through a Virtualization.framework NAT NIC (the allowlist no longer applies; a warning is printed and an audit record written). In allowlist and offline modes the guest still has no NIC, and SSH and the model gateway go over vsock.
- **Preflight and `doctor`:** check the helper's entitlement signature, Rosetta, whether Virtualization is available, the CPU and memory limits, APFS, and the unix socket path length (macOS allows 104 bytes), and show what this Mac allows.
- **`make`:** on a Mac builds the CLI and the signed helper together, so the pair always matches.

### Fixed

- **Disk corruption (root cause found):** the helper attached the sparse raw disk with the `.automatic` caching mode, and under memory pressure the guest could read back stale zeros, corrupting kernels, shims and libraries (a boot that hangs, or `apt-get` crashing with `SIGILL`). It now uses `.cached` with `synchronizationMode: .full`. Disabling `discard` is only an extra safeguard now.
- **Helper:** ignores SIGPIPE (a guest closing a connection no longer ends the VM); fixes the fd and connection object leak of every vsock connection; backs off when accept fails instead of spinning a core; clamps CPU and memory to what the Mac allows; forces a stop after 10 seconds so no VM process is left behind.
- **macOS backend:** fixes a `Wait`/`Stop` deadlock (a session that failed to start stayed in `starting`).
- **Project import race (present in V0.1):** a session became `ready` as soon as sshd answered, but cloud-init creates `/workspace` in its last stage, so the import sometimes failed (`import failed: exit status 2`). `run` and `start` now wait for the guest's first-boot setup before importing. Same test: 3 of 3 failures before the fix with the previous code, 6 of 6 successes after. The cost is a few seconds more per start.
- **`build.sh`:** the mirror source rule did not match on arm64.
- **cloud-init:** when systemd already serves vsock port 22, our own socat bridge is skipped instead of failing to bind.

### Verification

| Item | Status |
| --- | --- |
| Linux x86 regression (what V0.1 verified) | **Verified**: `doctor`, data of a saved desktop session kept, `pull` reviews first, audit chain, the disk of a session without `--keep` deleted on stop, allowlist blocking, offline |
| The race fix | **Verified**: 6 imports in a row, the `start` resume path works |
| Unit tests | `go vet` and `go test -race` (11 packages) pass; `vet` and `build` for `darwin/arm64` pass |
| macOS: headless session | Verified by the project owner on an Apple silicon Mac (image build, verification boot, vsock SSH, file import, proxy, model gateway bridge, CC-Monitor, audit chain). **Cannot be re-checked on this repository's Linux machine** |
| macOS: desktop window | Verified by the project owner on an Apple silicon Mac (`glasshouse run --gui`: GDM auto-login, the GNOME desktop shows in the helper's window). **Cannot be re-checked on this repository's Linux machine** |
| macOS: `host` network mode | Unit tests pass; for what was tried on the Mac, see the README |

### Tried but not kept

- **`network.upstream_proxy`** (send allowlisted connections through an upstream proxy such as Clash by hostname, to get around fake-ip DNS): on Linux the CONNECT tunnel was established but the TLS handshake in the guest stalled until the timeout. The cause was not found, so it was removed from the patch. With fake-ip DNS use `network.mode: host`, or add the allowlisted domains to the proxy's `fake-ip-filter`.

## V0.1 (2026-11-17, tag `V0.1`, commit `4254be6`)

**Linux x86 platform functionally verified.** Environment: Linux amd64, KVM, QEMU, Ubuntu 26.04.1 VM.

### Features

- **Isolation:** runs AI agents in a hardware-virtualized Ubuntu 26.04 VM (QEMU/KVM) with no host directory mounted; the project is packed into `/workspace`, skipping `.env*`, `*.pem`, `id_*`, `.git/config` and symlinks pointing outside it.
- **Network policy:** `allowlist` (default: only allowlisted domains, private and cloud-metadata addresses refused, egress capped), `offline`, and `host` (the host's network directly, explicit, never selectable by a project policy). A project policy can only tighten. The default allowlist includes the Claude login domains.
- **Bringing results back:** `glasshouse pull` shows a diff before writing; the host parses the tar itself, rejecting absolute paths, `..`, hard links and device files, stripping setuid bits and never writing through symlinks.
- **Audit:** a hash-chained audit log on the host; if it cannot be written, networking and export are refused (fail closed).
- **Two launch modes:** `--gui` (GNOME desktop) and `--shell` (light, terminal only).
- **Images:** `headless` and `desktop`; `build.sh` verifies Canonical's signature, supports the Tsinghua and USTC mirrors (`--mirror auto|tuna|ustc`), and writes the image to a temporary file before renaming it into place.
- **Preinstalled software:** the build installs the newest Claude Code (subscription login works), ccstatusline (with the host's own configuration) and CC-Monitor at a pinned commit; CC-Monitor's hooks are written into Claude Code's system-wide managed settings during its installation, where the `agent` user cannot edit them.
- **Kept sessions:** `--keep` with `glasshouse start <session>`: data is still there after a stop; a session whose supervisor vanished (`dead`) can be started too.
- **Convenience scripts:** `start.sh` (saves the desktop session, one-command start) and `qemu-ssh` (one command into a root shell).
- **Desktop:** desktop sessions never lock, blank or suspend (the agent has no password, so a locked screen could not be unlocked); `glasshouse gui` connects `remote-viewer` through a one-shot socat bridge.
- **Documentation:** English and Chinese README and usage manual, three demo screenshots.

### Verification

**Verified:** building both images, the GNOME desktop and auto-login, the light shell and `qemu-ssh`, stopping and starting a kept session, subscription login of Claude Code, networking in `host` mode, project import, `pull` reviewing before writing back, the audit chain check, the CC-Monitor service and hooks.

**Not verified:** macOS (Apple silicon), building an arm64 image, CC-Monitor blocking in Enforcing mode with the real Claude Code, API-key mode through the model gateway.

### Known issue (fixed in V0.2)

- Project import sometimes failed (`import failed: exit status 2`): the session became `ready` before `/workspace` existed. The "project import" line in the V0.1 tag message was therefore not stable.
