# Glasshouse User Manual

English | [简体中文](./USAGE.zh-CN.md)

This manual follows the order of real use: prepare the host → build images → start → get into the VM → log in to Claude → bring results back → change the configuration (CPU, memory, disk, network) → stop and clean up → troubleshoot.

Items marked "verified" were really run on this machine (Linux amd64, KVM, QEMU); items marked "not verified" were not.

## 1. Prepare the host

| Needed | Notes |
| --- | --- |
| Linux amd64 + KVM | `/dev/kvm` must be usable; without it only `--allow-tcg` software emulation works, and it is extremely slow |
| QEMU | `qemu-system-x86_64`, `qemu-img` |
| For building images | `libguestfs-tools` (`virt-customize`, `virt-sysprep`), `isc-dhcp-client`, `git`, `curl`, `gpgv` |
| Desktop window (optional) | `virt-viewer` (provides `remote-viewer`); without it the desktop still runs, you just connect to VNC yourself |
| Go 1.24 | only to compile it yourself; no third-party dependencies |

```bash
make build                 # produces bin/glasshouse
bin/glasshouse doctor      # should end with: all checks passed
```

The `credentials.llm_gateway.key_ref is not set` line in `doctor` is only a note: with a subscription login you do not set it.

macOS uses Virtualization.framework. That code is written but has **never been compiled or run**, so this manual does not cover it.

## 2. Build the images

Images live in `~/.local/share/glasshouse/images/`, in two kinds:

| Image | Purpose | Size |
| --- | --- | --- |
| `headless` | light, no desktop, for the SSH shell mode | about 2.3 GB |
| `desktop` | GNOME desktop | about 3.9 GB |

```bash
export CC_MONITOR_REF=1b3516f5b5152f882daad6e6d5c2506bf166a35a   # pinned CC-Monitor commit, must be a 40-hex hash
bash ./image/build.sh --profile headless
bash ./image/build.sh --profile desktop
```

- Each image takes roughly 10–30 minutes. The build temporarily needs about 3–8 GB of disk and cleans up afterwards.
- Claude Code is installed at the newest release (`CLAUDE_CODE_VERSION=latest`), and so is ccstatusline. The order is Claude Code, then ccstatusline, then CC-Monitor, which hooks Claude Code while it is installed.
- On networks in China use `--mirror auto|tuna|ustc`, and `NPM_REGISTRY=https://registry.npmmirror.com` for npm. `auto` measures and keeps the official source when it is not slow.
- The image is written to a temporary file and renamed into place, so a build also works while the old image is in use (verified).
- **Rebuilding an image invalidates every session whose disk was kept**, see §8.

## 3. Start

### 3.1 The easy way: `./start.sh` (desktop, data kept)

```bash
./start.sh                 # first run creates a desktop session; every later run boots it again
./start.sh ./myproject     # on the first run, copy the project into /workspace
./start.sh status          # show the state
./start.sh stop            # shut down, the disk is kept
./start.sh reset           # delete the session and all its data (type the session id to confirm)
```

- The session id is saved in `~/.local/share/glasshouse/desktop-session`.
- Installed software, files and the Claude login survive stop, start again, and a killed process (verified).
- Running `./start.sh` while the VM is already running just reopens the desktop window.

### 3.2 Using `glasshouse` directly

| Goal | Command |
| --- | --- |
| Light shell, throwaway | `glasshouse run --shell ./proj` |
| Desktop, throwaway | `glasshouse run --gui ./proj` |
| Keep the data (start it again later) | add `--keep`: `glasshouse run --keep --shell ./proj` |
| No network | add `--offline` |
| Run one command instead of a shell | `glasshouse run ./proj -- claude` |
| Boot a kept session again | `glasshouse start <session>` |

Notes:

- Without `--shell` or `--gui`, `vm.profile` in the policy decides (default `desktop`). A command-line flag cannot loosen the policy: `--gui` is refused when the policy forces headless.
- The project directory is copied into the VM's `/workspace`, not mounted. `.env*`, `*.pem`, `id_*`, `.git/config` and symlinks pointing outside the directory are not brought in.
- In shell mode, **leaving the shell does not stop the VM**; use `glasshouse stop`.
- **A session without `--keep` has its disk deleted automatically when it stops**, and everything in it is gone. Add `--keep` to keep it, and use `start` to boot it again.
- `glasshouse start` reuses the policy saved when the session was created (CPU and memory are the values from then too) and installs a fresh SSH key and token on every start. A session shown as `dead` after a host reboot can be started too (verified by killing the processes to simulate it).

### 3.3 The desktop window

When a desktop session is ready, a window is opened automatically, which needs `remote-viewer`:

```bash
sudo apt install virt-viewer socat
glasshouse gui <session>      # reopen the window at any time
```

It needs `remote-viewer` and `socat` (`sudo apt install virt-viewer socat`). Do **not** run `remote-viewer vnc+unix://…` by hand: virt-viewer 11 cannot open it and fails with `vnc_display_open_host: assertion 'host != NULL' failed`. `glasshouse gui` starts a one-shot `socat` bridge on 127.0.0.1 (random port, one connection, closed after a minute if nobody connects) and points `remote-viewer` at it. The display has no password, so while that port is open any local user could connect. I confirmed that the bridge connects and stays up; I have not seen the desktop picture myself, only that gdm3 and gnome-shell run and `agent` is logged in automatically.

## 4. Get into the VM

```bash
glasshouse ssh                              # root shell (the session can be omitted when only one is running)
glasshouse ssh --user agent                 # the unprivileged user agent (owns /workspace, home /home/agent)
glasshouse ssh --user agent s-abc123        # a specific session
glasshouse ssh --user agent s-abc123 -- ls /workspace
```

- **`--user` must come before the session id.** After it, it has no effect (found out on the real machine).
- With several sessions running you must name one.
- **There are no passwords.** `root` and `agent` are locked and SSH password login is off; the desktop logs in as `agent` automatically and `agent` has passwordless `sudo`. The VNC display has no password either. If something does ask for a password (a locked screen, for instance), see the troubleshooting table.
- Root inside the VM is not a security boundary; the VM itself is. The policy `ssh.root: false` disables root login.
- A quoted compound command (such as `-- 'a; b'`) is taken as one program name; for a longer script use `-- bash -s < script-file`.

## 5. Log in to Claude and start working

The image has Claude Code (newest release) and ccstatusline preinstalled, with the status line set up like your host.

```bash
glasshouse ssh --user agent
cd /workspace
claude            # the first run prints a login link; authorize it in your browser with your subscription account
```

- Subscription login is verified: the screenshots in the README show Claude Code 2.1.326 running in the VM on a Claude Pro login (authorized in a browser by the project owner, with `network.mode: host`).
- The credentials are stored in the VM (`~/.claude/.credentials.json`). In a session with a kept disk they stay; if the VM is compromised they can be read, which is a known risk of subscription login.
- The default network policy allows `claude.ai`, `platform.claude.com` and `api.anthropic.com`.
- Auto-update is off inside the VM; rebuild the image to get a newer Claude Code.
- The account and usage items of ccstatusline have content only after login; before that they show `[No credentials]` (verified).
- The CC-Monitor hooks are in the image (`/etc/claude-code/managed-settings.json`, owned by root, not editable by agent). Claude Code does run them: in the second README screenshot CC-Monitor shows the Claude Code session as monitored, with 5 audit events (the mode was Permissive, which only logs and never blocks). Blocking in Enforcing mode has not been tried with the real Claude Code; only the hook script itself was (`rm -rf /` was blocked, `ls` allowed). Its UI: `glasshouse monitor`, or the browser inside the desktop VM at `127.0.0.1:9999`.

## 6. Bring the results back

```bash
glasshouse pull                  # shows a diff first, writes to the host project only after you confirm
glasshouse pull --yes            # no prompt (only when the policy files.export is review)
glasshouse pull --delete         # also delete files that no longer exist in the VM
glasshouse pull s-abc123 ./out   # a given session and target directory
```

The host parses the exported tar itself: it rejects absolute paths, `..`, hard links and device files, strips setuid bits, and never writes through symlinks. **`pull` before you stop, rebuild an image, `reset` or `rm`.**

## 7. Change the configuration

### 7.1 Files and precedence

| Layer | Location | What it can do |
| --- | --- | --- |
| User policy | `~/.config/glasshouse/policy.yaml`, or `--policy FILE`, or the environment variable `GLASSHOUSE_POLICY` | sets the ceiling; can loosen or tighten |
| Project policy | `.glasshouse.yaml` in the project directory | **can only tighten**, never enlarge |
| Command-line flags | `--offline`, `--shell`, etc. | **can only tighten** |

See `docs/policy.example.yaml` for the full example. Check a file after editing:

```bash
glasshouse policy check ~/.config/glasshouse/policy.yaml
```

This machine currently has only:

```yaml
version: 1
network:
  mode: host
```

### 7.2 Add CPU, memory and disk

Add a `vm` section to the user policy:

```yaml
version: 1
vm:
  profile: desktop     # headless | desktop
  cpus: 8              # 1–64, default 4
  memory: 16GiB        # at least 512MiB, default 8GiB
  disk: 60GiB          # at least 4GiB, default 40GiB
  timeout: 8h          # the session stops itself after this; 0 means unlimited
network:
  mode: host
```

**Three things you must know:**

1. **Existing sessions do not change.** CPU, memory and disk are written into the session record when it is created, and `start` and `./start.sh` use those values. For the new configuration to apply you need a new session:
   ```bash
   glasshouse pull            # bring your work back first
   ./start.sh reset           # delete the old session (the software and Claude login in it are lost)
   ./start.sh                 # create it with the new configuration
   ```
2. **The disk of an existing session cannot be grown.** There is no command for it.
3. **Do not exceed the host.** This machine has 32 cores, 30 GB of memory, about 23 GB available. Too much memory makes the host swap, and processes such as Clash suffer. The code checks only the lower bound of memory and has **no upper limit check**; I have not verified what happens with a huge value.

There is no way to change resources without recreating the session. If you need it, `glasshouse start` could get `--cpus` and `--memory` override flags; that is not done yet.

### 7.3 Network modes

```yaml
network:
  mode: allowlist     # allowlist | offline | host
  allow:
    - github.com:443
    - pypi.org:443
  max_egress: 2GiB    # 0 means unlimited
```

| Mode | Meaning |
| --- | --- |
| `allowlist` (default) | only the hosts and ports in `allow` are reachable; private and metadata addresses are refused; egress has a cap; all of it is audited |
| `offline` | no outbound network |
| `host` | the VM uses the host's network directly. The allowlist, the private-address blocking and the egress cap **no longer apply**; the VM can reach anything the host can, including the LAN and `10.0.2.2` (host loopback). It is an explicit policy item, a project policy cannot turn it on, and a warning is printed and an audit record written at start |

This machine uses `host` because Clash's fake-ip DNS returns `198.18.0.x`, which `allowlist` mode refuses as a non-public address. Real connectivity in `host` mode is verified: the subscription login and a model reply worked from inside the VM (see the README screenshots).

### 7.4 Other common settings

| Setting | Meaning |
| --- | --- |
| `files.mode: copy\|none` | whether the project is copied into the VM |
| `files.export: review\|auto\|deny` | on `pull`: review first, write automatically, or forbid |
| `files.deny_patterns` | file patterns that are not brought into the VM |
| `files.max_import` | size limit for the project import, default 500MiB |
| `ssh.root` | whether root login is allowed |
| `monitor.cc_monitor` | whether CC-Monitor is on; when off, the session deletes `/etc/claude-code/managed-settings.json` |
| `credentials.llm_gateway.key_ref` | optional. Only when set does traffic go through the model gateway on the host (API-key mode); when unset, log in with your subscription inside the VM |

### 7.5 Environment variables

| Variable | Effect |
| --- | --- |
| `GLASSHOUSE_HOME` | data directory, default `~/.local/share/glasshouse`. A path that is too long breaks the unix sockets, so use a short one |
| `GLASSHOUSE_POLICY` | path of the user policy file |
| `GLASSHOUSE_BIN` | the `glasshouse` executable that `start.sh` uses |

## 8. Stop, keep and clean up

```bash
glasshouse ls                 # states: starting → ready → stopping → stopped; failed and dead below
glasshouse stop [session]     # stop; for a session without --keep the disk is deleted with it
glasshouse rm <session>       # delete a stopped session's directory, including the audit log
```

| You want | Do |
| --- | --- |
| Data still there after shutdown | `run --keep` or `./start.sh`, then boot it with `start` |
| A clean throwaway environment | no `--keep`; it is emptied when it stops |
| State is `dead` | the supervisor is gone (host reboot, killed). A kept-disk session can be `start`ed; clean up others with `rm` |
| State is `failed` | the start failed; look at `glasshouse logs --console` first; a kept-disk session can try `start` again |

**A kept disk cannot be used after the image is rebuilt.** The disk is layered on the base image of that time; once the image is rebuilt, `start` refuses and says why. It does not delete the data automatically, but the data cannot be recovered either, so `pull` before rebuilding.

## 9. Logs and audit

```bash
glasshouse logs                 # the audit log on the host
glasshouse logs --follow        # keep printing
glasshouse logs --verify        # verify the hash chain (verified; the test that a hand-edited log must fail is not done yet)
glasshouse logs --console       # the VM's serial console, look here first when boot fails
```

The audit log is on the host and every record carries the hash of the previous one. When it cannot be written, network connections and file exports are refused (fail closed).

## 10. Troubleshooting

| Symptom | What to do |
| --- | --- |
| `doctor` reports a missing tool | install it per §1; `doctor` says which |
| No `/dev/kvm` | check BIOS virtualization and user permissions; use `--allow-tcg` only if there is really none, it is extremely slow |
| Start times out or `failed` | `glasshouse logs --console`; `~/.local/share/glasshouse/sessions/<session>/` has `supervisor.log` and `qemu.log` |
| `start` says the base image has been rebuilt | the image was rebuilt and the old disk cannot be used; recreate after `reset` or `rm` |
| An allowlisted domain is unreachable under `allowlist` | look for `net.deny` in `glasshouse logs`; if it is `198.18.0.x` that is Clash fake-ip, use `network.mode: host` or change DNS |
| Socket path too long | set a shorter `GLASSHOUSE_HOME` |
| No gnome-shell right after the desktop starts | after a restart GNOME needs a few dozen seconds more to come up (observed) |
| Desktop is black ("Display output is not active.") or asks for a password | the screen was locked after 5 idle minutes and the users have no password. From the host: `glasshouse ssh <session> -- loginctl unlock-sessions`, then move the mouse. New sessions never lock or suspend; for an older one see the README troubleshooting entry (turn off `lock-enabled` and the idle suspend with `gsettings`), or set a password with `sudo passwd agent` |
| Cleaning up a leftover QEMU | look for `dead` in `glasshouse ls`; once you are sure no session uses it, `pkill -x qemu-system-x86_64` |
| Build fails | look at the last `==>` step in the build log; `virt-customize` prints no fine-grained progress, so a long silence does not mean it is stuck |

## 11. Known limitations

- `network.mode: host` loosens isolation; use it only when you trust what runs in the VM.
- The subscription login token sits in an untrusted VM.
- Under `allowlist` the guest can only use HTTP(S); bare SSH to GitHub does not work, use HTTPS for git.
- The disk cannot be grown, and CPU and memory cannot be changed, for an existing session.
- `monthly_budget` is only parsed, not enforced.
- Not verified: blocking in CC-Monitor's Enforcing mode with the real Claude Code, the model gateway (API-key mode), arm64, macOS.
