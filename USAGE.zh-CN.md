# Glasshouse 使用手册

[English](./USAGE.md) | 简体中文

本文按"从零到日常使用"的顺序写：准备环境 → 构建镜像 → 启动 → 进入虚拟机 → 登录 Claude → 取回成果 → 修改配置（CPU、内存、磁盘、网络）→ 停止与清理 → 排错。

标注"已验证"的内容在本机（Linux amd64、KVM、QEMU）上真实跑过；标注"未验证"的没有。

## 1. 准备环境

| 需要 | 说明 |
| --- | --- |
| Linux amd64 + KVM | `/dev/kvm` 必须可用；没有的话只能用 `--allow-tcg` 软件模拟，极慢 |
| QEMU | `qemu-system-x86_64`、`qemu-img` |
| 构建镜像用 | `libguestfs-tools`（`virt-customize`、`virt-sysprep`）、`isc-dhcp-client`、`git`、`curl`、`gpgv` |
| 桌面窗口（可选） | `virt-viewer`（提供 `remote-viewer`），没有它也能用桌面，只是要自己连 VNC |
| Go 1.24 | 只在自己编译时需要，无第三方依赖 |

```bash
make build                 # 生成 bin/glasshouse
bin/glasshouse doctor      # 检查环境，应该显示 all checks passed
```

`doctor` 里那条 `credentials.llm_gateway.key_ref is not set` 只是提示：你用订阅账号登录时本来就不设它。

macOS 用 Virtualization.framework，代码写了但**从未编译和运行过**，本文不涉及。

## 2. 构建镜像

镜像放在 `~/.local/share/glasshouse/images/`，分两种：

| 镜像 | 用途 | 大小 |
| --- | --- | --- |
| `headless` | 轻量，没有桌面，SSH shell 模式用 | 约 2.3 GB |
| `desktop` | GNOME 桌面 | 约 3.9 GB |

```bash
export CC_MONITOR_REF=1b3516f5b5152f882daad6e6d5c2506bf166a35a   # CC-Monitor 固定提交，必须是 40 位哈希
bash ./image/build.sh --profile headless
bash ./image/build.sh --profile desktop
```

- 每个镜像大约 10–30 分钟，构建时临时占用约 3–8 GB 磁盘，结束后自动清理。
- 构建时 Claude Code 装最新版（`CLAUDE_CODE_VERSION=latest`），ccstatusline 也是最新版；顺序是先 Claude Code，再 ccstatusline，再 CC-Monitor 并对 Claude Code 做 hook。
- 国内网络可用 `--mirror auto|tuna|ustc`，`NPM_REGISTRY=https://registry.npmmirror.com` 加速 npm。`auto` 会实测，官方源不慢就用官方源。
- 镜像先写成临时文件再改名到位，所以旧镜像被占用时也能构建（已验证）。
- **重建镜像会让所有"保留磁盘"的会话失效**，见 §8。

## 3. 启动

### 3.1 最省事：`./start.sh`（桌面，数据保留）

```bash
./start.sh                 # 第一次创建桌面会话；之后每次都是重新启动它
./start.sh ./myproject     # 第一次创建时把项目复制到 /workspace
./start.sh status          # 看状态
./start.sh stop            # 关闭，磁盘保留
./start.sh reset           # 删除会话和全部数据（输入会话 ID 确认）
```

- 会话 ID 保存在 `~/.local/share/glasshouse/desktop-session`。
- 装的软件、文件、Claude 登录在停止、再启动、强杀进程后都还在（已验证）。
- 已在运行时再执行 `./start.sh`，只是重新打开桌面窗口。

### 3.2 直接用 `glasshouse`

| 目的 | 命令 |
| --- | --- |
| 轻量 shell，用完即弃 | `glasshouse run --shell ./proj` |
| 桌面，用完即弃 | `glasshouse run --gui ./proj` |
| 保留数据（以后可再启动） | 加 `--keep`：`glasshouse run --keep --shell ./proj` |
| 断网 | 加 `--offline` |
| 不进 shell，只跑一条命令 | `glasshouse run ./proj -- claude` |
| 重新启动已保留的会话 | `glasshouse start <会话>` |

要点：

- 不加 `--shell`/`--gui` 时，由策略里的 `vm.profile` 决定（默认 `desktop`）。命令行参数不能放宽策略：策略强制 headless 时 `--gui` 会被拒绝。
- 项目目录被复制到虚拟机的 `/workspace`，不会被挂载；`.env*`、`*.pem`、`id_*`、`.git/config` 和指向目录外的软链接不会带入。
- shell 模式下**退出 shell 不会关闭虚拟机**，要 `glasshouse stop`。
- **不加 `--keep` 的会话，停止时磁盘会被自动删除**，里面的东西全部消失。要保留就加 `--keep`，并用 `start` 再启动。
- `glasshouse start` 的会话沿用创建时保存的策略（CPU、内存也是创建时的值）；每次启动换新的 SSH 密钥和令牌。主机重启后显示为 `dead` 的会话也能启动（已验证，用强杀进程模拟）。

### 3.3 桌面窗口

桌面模式就绪后会自动尝试打开窗口，需要 `remote-viewer`：

```bash
sudo apt install virt-viewer socat
glasshouse gui <会话>      # 之后随时重新打开窗口
```

需要 `remote-viewer` 和 `socat`（`sudo apt install virt-viewer socat`）。**不要**手动运行 `remote-viewer vnc+unix://…`：virt-viewer 11 打不开它，会报 `vnc_display_open_host: assertion 'host != NULL' failed`。`glasshouse gui` 会在 127.0.0.1 上临时起一个一次性的 `socat` 桥（随机端口，只接受一个连接，一分钟内没人连就关闭），再让 `remote-viewer` 连它。显示没有密码，这个端口开着的时候，本机任何用户都能连。我确认了这个桥能连上并保持连接；我自己没看到过桌面画面，只确认了 gdm3、gnome-shell 在运行且 agent 自动登录。

## 4. 进入虚拟机

```bash
glasshouse ssh                              # root shell（只有一个运行中会话时可省略会话）
glasshouse ssh --user agent                 # 普通用户 agent（拥有 /workspace，家目录 /home/agent）
glasshouse ssh --user agent s-abc123        # 指定会话
glasshouse ssh --user agent s-abc123 -- ls /workspace
```

- **`--user` 必须写在会话编号前面**，写在后面不会生效（真机上踩过）。
- 多个会话同时运行时必须指名。
- **没有任何密码。** `root` 和 `agent` 都是锁定的，SSH 不允许密码登录；桌面自动以 `agent` 登录，`agent` 的 `sudo` 不要密码。VNC 显示也没有密码。如果有东西要密码（比如锁屏），看排错表。
- 虚拟机里的 root 不是安全边界，虚拟机本身才是。策略 `ssh.root: false` 可以禁掉 root 登录。
- 带引号的复合命令（如 `-- 'a; b'`）会被当成一个程序名，复杂脚本用 `-- bash -s < 脚本文件`。

## 5. 登录 Claude 并开始工作

镜像里预装了 Claude Code（最新版）和 ccstatusline，并且已经按你宿主机的配置设置好状态栏。

```bash
glasshouse ssh --user agent
cd /workspace
claude            # 首次运行会给出登录链接，用你的订阅账号在浏览器里授权
```

- 订阅登录已验证：README 里的截图显示 Claude Code 2.1.326 在虚拟机里用 Claude Pro 账号登录运行（由项目所有者在浏览器里授权，`network.mode: host`）。
- 登录凭证存在虚拟机里（`~/.claude/.credentials.json`）。保留磁盘的会话里它会一直在；虚拟机被攻破时它可能被读走，这是订阅登录模式的已知风险。
- 默认网络策略允许 `claude.ai`、`platform.claude.com`、`api.anthropic.com`。
- 虚拟机里自动更新是关闭的，要新版 Claude Code 就重建镜像。
- ccstatusline 的账号、用量那几项登录后才有内容，没登录时显示 `[No credentials]`（已验证）。
- CC-Monitor 的 hook 在镜像里（`/etc/claude-code/managed-settings.json`，root 属主，agent 改不了）。Claude Code 确实在执行它们：README 第二张截图里 CC-Monitor 显示该 Claude Code 会话已被监控，有 5 条审计事件（当时是 Permissive 模式，只记录、不拦截）。Enforcing 模式下对真实 Claude Code 的拦截没试过，只试过 hook 脚本本身（`rm -rf /` 被拦截，`ls` 放行）。界面：`glasshouse monitor`，或桌面虚拟机里的浏览器打开 `127.0.0.1:9999`。

## 6. 取回成果

```bash
glasshouse pull                  # 先显示 diff，确认后才写入宿主机项目目录
glasshouse pull --yes            # 不询问（仅当策略 files.export 为 review 时可用）
glasshouse pull --delete         # 同时删除虚拟机里已经不存在的文件
glasshouse pull s-abc123 ./out   # 指定会话和目标目录
```

宿主机自己解析导出的 tar：拒绝绝对路径、`..`、硬链接、设备文件，去掉 setuid 位，不会顺着软链接写文件。**停止、重建镜像、`reset`、`rm` 之前先 `pull`。**

## 7. 修改配置

### 7.1 配置文件和优先级

| 层 | 位置 | 能做什么 |
| --- | --- | --- |
| 用户策略 | `~/.config/glasshouse/policy.yaml`，或 `--policy 文件`，或环境变量 `GLASSHOUSE_POLICY` | 设定上限，可放宽也可收紧 |
| 项目策略 | 项目目录下的 `.glasshouse.yaml` | **只能收紧**，不能放大 |
| 命令行参数 | `--offline`、`--shell` 等 | **只能收紧** |

完整示例见 `docs/policy.example.yaml`。改完先检查：

```bash
glasshouse policy check ~/.config/glasshouse/policy.yaml
```

你这台机器当前只有：

```yaml
version: 1
network:
  mode: host
```

### 7.2 增加 CPU、内存、磁盘

在用户策略里加 `vm` 段：

```yaml
version: 1
vm:
  profile: desktop     # headless | desktop
  cpus: 8              # 1–64，默认 4
  memory: 16GiB        # 至少 512MiB，默认 8GiB
  disk: 60GiB          # 至少 4GiB，默认 40GiB
  timeout: 8h          # 超时后会话自动停止；0 表示不限
network:
  mode: host
```

**必须知道的三点：**

1. **已有会话不会变。** CPU、内存、磁盘在创建会话时写入记录，`start` 和 `./start.sh` 用的是当时的值。要新配置生效就要新建会话：
   ```bash
   glasshouse pull            # 先取回成果
   ./start.sh reset           # 删除旧会话（里面的软件和 Claude 登录会丢）
   ./start.sh                 # 用新配置创建
   ```
2. **磁盘不能给已有会话扩容**，现在没有这个命令。
3. **别超过宿主机。** 这台机器 32 核、30 GB 内存、可用约 23 GB。内存给太多宿主机会换页，Clash 等进程也会受影响。代码只检查内存下限，**没有上限检查**，填很大的值会怎样我没验证。

没有"不重建会话就改资源"的功能；如果需要，可以给 `glasshouse start` 加 `--cpus`、`--memory` 覆盖参数，这个还没做。

### 7.3 网络模式

```yaml
network:
  mode: allowlist     # allowlist | offline | host
  allow:
    - github.com:443
    - pypi.org:443
  max_egress: 2GiB    # 0 表示不限
```

| 模式 | 含义 |
| --- | --- |
| `allowlist`（默认） | 只能访问 `allow` 里的域名和端口，私网和元数据地址被拒绝，出站有流量上限，都记审计 |
| `offline` | 没有出站网络 |
| `host` | 虚拟机直接用宿主机的网络。白名单、私网封锁、流量上限**都不再生效**，虚拟机能访问宿主机能访问的一切，包括局域网和 `10.0.2.2`（宿主机环回）。是显式策略项，项目策略不能开启，启动时有警告并写审计 |

这台机器用 `host`，原因是 Clash 的 fake-ip DNS 返回 `198.18.0.x`，`allowlist` 模式会把它当成非公网地址拒绝。`host` 模式的真实联网已验证：在虚拟机里完成了订阅登录并拿到了模型回复（见 README 截图）。

### 7.4 其他常用项

| 配置 | 含义 |
| --- | --- |
| `files.mode: copy\|none` | 是否把项目复制进虚拟机 |
| `files.export: review\|auto\|deny` | `pull` 时先审阅、自动写入、还是禁止 |
| `files.deny_patterns` | 不带入虚拟机的文件模式 |
| `files.max_import` | 项目导入大小上限，默认 500MiB |
| `ssh.root` | 是否允许 root 登录 |
| `monitor.cc_monitor` | 是否启用 CC-Monitor；关闭时会话会删除 `/etc/claude-code/managed-settings.json` |
| `credentials.llm_gateway.key_ref` | 可选。设了才会走宿主机上的模型网关（API Key 模式）；不设就在虚拟机里用订阅登录 |

### 7.5 环境变量

| 变量 | 作用 |
| --- | --- |
| `GLASSHOUSE_HOME` | 数据目录，默认 `~/.local/share/glasshouse`。路径太长会导致 unix socket 失败，要用短路径 |
| `GLASSHOUSE_POLICY` | 用户策略文件路径 |
| `GLASSHOUSE_BIN` | `start.sh` 使用的 `glasshouse` 可执行文件 |

## 8. 停止、保留和清理

```bash
glasshouse ls                 # 状态：starting → ready → stopping → stopped；failed、dead 见下
glasshouse stop [会话]         # 停止；没加 --keep 的会话磁盘随之删除
glasshouse rm <会话>           # 删除已停止会话的目录，包括审计日志
```

| 想要 | 做法 |
| --- | --- |
| 关机后数据还在 | `run --keep` 或 `./start.sh`，再用 `start` 启动 |
| 一次性干净环境 | 不加 `--keep`，停止后自动清空 |
| 状态是 `dead` | 监督进程消失（宿主机重启、被杀）。保留磁盘的会话可 `start`；其他的 `rm` 清理 |
| 状态是 `failed` | 启动失败，先 `glasshouse logs --console`；保留磁盘的会话可再试 `start` |

**重建镜像后保留的磁盘不能再用。** 磁盘叠在当时的基础镜像上，镜像被重建后 `start` 会拒绝并说明原因，不会自动删数据，但这些数据也找不回了。所以重建前先 `pull`。

## 9. 日志与审计

```bash
glasshouse logs                 # 宿主机上的审计日志
glasshouse logs --follow        # 持续输出
glasshouse logs --verify        # 校验哈希链（已验证；手改日志后必须失败的测试还没做）
glasshouse logs --console       # 虚拟机串口控制台，启动失败先看这个
```

审计日志在宿主机，每条记录带上一条的哈希；写不进去时网络连接和文件导出会被拒绝（失败即关闭）。

## 10. 排错

| 现象 | 处理 |
| --- | --- |
| `doctor` 报缺工具 | 按 §1 安装，`doctor` 会指出缺哪个 |
| 没有 `/dev/kvm` | 检查 BIOS 虚拟化和用户权限；实在没有才用 `--allow-tcg`，会极慢 |
| 启动超时或 `failed` | `glasshouse logs --console`；`~/.local/share/glasshouse/sessions/<会话>/` 下有 `supervisor.log`、`qemu.log` |
| `start` 说 base image has been rebuilt | 镜像被重建过，旧磁盘不能用；`reset` 或 `rm` 后重新创建 |
| `allowlist` 下白名单域名连不上 | 看 `glasshouse logs` 里的 `net.deny`；如果是 `198.18.0.x`，是 Clash fake-ip，用 `network.mode: host` 或换 DNS |
| socket 路径太长 | 设短一点的 `GLASSHOUSE_HOME` |
| 桌面刚启动时没有 gnome-shell | 重启后要多等几十秒，GNOME 才起来（已观察到） |
| 桌面一片黑（"Display output is not active."）或要输入密码 | 空闲 5 分钟后屏幕被锁定，而用户没有密码。从宿主机：`glasshouse ssh <会话> -- loginctl unlock-sessions`，再动鼠标。新会话不会再锁屏或挂起；更早的会话见 README 排错里的条目（用 `gsettings` 关掉 `lock-enabled` 和空闲挂起），或者 `sudo passwd agent` 设个密码 |
| 清理残留 QEMU | `glasshouse ls` 看有没有 `dead`；确认没有会话在用再 `pkill -x qemu-system-x86_64` |
| 构建失败 | 看构建日志里最后的 `==>` 步骤；`virt-customize` 不输出细粒度进度，长时间无输出不代表卡死 |

## 11. 已知限制

- `network.mode: host` 放宽隔离，只在你信任虚拟机里运行的东西时使用。
- 订阅登录令牌在不可信的虚拟机里。
- `allowlist` 模式下 guest 只能走 HTTP(S)，裸 SSH 到 GitHub 不通，git 用 HTTPS。
- 磁盘不能给已有会话扩容，CPU 和内存也不能给已有会话修改。
- `monthly_budget` 只解析不执行。
- 未验证：CC-Monitor Enforcing 模式下对真实 Claude Code 的拦截、模型网关（API Key 模式）、arm64、macOS。
