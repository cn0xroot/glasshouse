# 更新记录

[English](./CHANGELOG.md) | 简体中文

按版本记录用户能感受到的变化。每一项都标明了验证状态：**已验证** 是在真机上跑过，**未验证** 是只写了代码或只有单元测试。逐项的代码修改过程见本地笔记 `docs/CHANGELOG.md`（已列入 `.gitignore`，不在仓库中）。

## 未发布（V0.2 之后）

- **修复：`glasshouse monitor` 在 `~/.ssh/config` 开启 `ControlMaster` 时超时。** 所有会话用同一个主机别名 `glasshouse`（macOS 后端），ssh 会复用另一个会话的 master，`monitor.sock` 的转发就永远不出现。现在所有 ssh 调用都带 `-o ControlMaster=no -o ControlPath=none`。（来自 Mac 端补丁。）Linux 上用 `ssh -G` 验证了命令行选项能盖过配置文件里的 `ControlMaster auto`；没有复现真正的 master 冲突，因为 Linux 会话的端口各不相同。
- **macOS：** 隧道的 ssh 会为每个转发连接打印一行无害的 `setsockopt TCP_NODELAY` 错误，现在在 macOS 上过滤掉。**Linux 保持原样**（ssh 的 stderr 直接指向文件）。
- **修复：退出时可能卡死。** 补丁原本把 ssh 的 stderr 改成 `io.Pipe`，退出时 `waitCmd` 的 goroutine 和清理用的 `defer` 会同时对隧道调用 `Wait`，两者抢同一个"拷贝结束"信号，输的一方永远等下去。独立复现：管道 300 次里卡住 5 次，文件 0 次。现在隧道只 `Wait` 一次，结果通过一个结束后关闭的 channel 交给所有接收者。

## V0.2（2026-11-17，标签 `V0.2`）

在 `V0.1` 之上，主要提交 `3f8429f`、`5f3716d`。主要内容：支持 Apple 芯片 Mac，并修复一个 V0.1 就有的竞态。

### 新增：Apple 芯片（macOS）支持

- **arm64 镜像构建** `image/build-mac.sh` 和 `image/provision-guest.sh`：在 Mac 上基于 Ubuntu 26.04 的 **arm64** cloud image，在 Virtualization.framework 虚拟机里构建镜像。构建后用一次无网络的启动来校验（`dpkg --verify` 加 EFI 引导文件比对），不通过就不写出镜像。Apple 芯片只能运行 arm64 guest，所以不能用 V0.1 的 x86 镜像。
- **macOS 的 `network.mode: host`：** guest 通过 Virtualization.framework 的 NAT 网卡直接使用 Mac 的网络（白名单不再生效，启动时警告并写入审计日志）。allowlist 和 offline 模式下 guest 仍然没有网卡，SSH 和模型网关走 vsock。
- **启动前预检和 `doctor`：** 检查 helper 的 entitlement 签名、Rosetta、Virtualization 是否可用、CPU 和内存上限、APFS、unix socket 路径长度（macOS 上限 104 字节），并显示本机允许的上限。
- **`make`：** 在 Mac 上一次编译命令行和已签名的 helper，二者版本总是配套。

### 修复

- **磁盘损坏（根因查明）：** helper 挂载稀疏 raw 磁盘时用了 `.automatic` 缓存模式，内存吃紧时 guest 会读回陈旧的全 0，造成内核、shim、库文件损坏（启动卡住，或 `apt-get` 因 `SIGILL` 崩溃）。改用 `.cached` 并开启 `synchronizationMode: .full`。`discard` 现在只是额外保险。
- **helper：** 忽略 SIGPIPE（guest 断开连接不再让 VM 退出）；修复每个 vsock 连接的 fd 和连接对象泄漏；accept 失败时退避，不再空转占满一个核；CPU 和内存按本机允许的范围收敛；强制停机改为 10 秒，避免 VM 进程残留。
- **macOS 后端：** 修复 `Wait`/`Stop` 的死锁（会话启动失败后卡在 `starting`）。
- **项目导入竞态（V0.1 就有）：** 会话在 sshd 能连上时就变成 `ready`，但创建 `/workspace` 的 cloud-init 命令在最后阶段才执行，导入偶发失败（`import failed: exit status 2`）。现在 `run` 和 `start` 在导入前先等 guest 的首次启动配置完成。同一个测试：修复前合并前的版本 3 次全部失败，修复后连续 6 次全部成功。代价是每次启动多等几秒。
- **`build.sh`：** 修复 arm64 上镜像源替换规则匹配不上。
- **cloud-init：** systemd 已经接管 vsock 22 端口时，跳过我们自己的 socat 转发，不再因端口占用而失败。

### 验证状态

| 项 | 状态 |
| --- | --- |
| Linux x86 回归（V0.1 已验证的功能） | **已验证**：`doctor`、保存的桌面会话数据保留、`pull` 先审阅、审计链、不带 `--keep` 停止后磁盘自动删除、白名单封锁、断网 |
| 竞态修复 | **已验证**：连续 6 次导入成功，`start` 恢复路径正常 |
| 单元测试 | `go vet`、`go test -race`（11 个包）通过；`darwin/arm64` 的 `vet` 和 `build` 通过 |
| macOS：headless 会话 | 由项目所有者在 Apple 芯片 Mac 上验证（构建镜像、校验启动、vsock SSH、文件导入、代理、模型网关转发、CC-Monitor、审计链）。**本仓库的 Linux 机器上无法复核** |
| macOS：桌面窗口 | 由项目所有者在 Apple 芯片 Mac 上验证（`glasshouse run --gui`：GDM 自动登录，GNOME 桌面在 helper 的窗口中正常显示）。**本仓库的 Linux 机器上无法复核** |
| macOS：`host` 网络模式 | 单元测试通过，Mac 端实测情况以 README 为准 |

### 试过但没有保留

- **`network.upstream_proxy`**（经 Clash 等上游代理按域名出站，用来解决 fake-ip 被拒）：在 Linux 上实测，CONNECT 隧道能建立，但 guest 里的 TLS 握手卡住超时，根因没有查明，所以从补丁里移除了。fake-ip 环境请用 `network.mode: host`，或者把白名单域名加进代理的 `fake-ip-filter`。

## V0.1（2026-11-17，标签 `V0.1`，提交 `4254be6`）

**Linux x86 平台功能验证正常。** 验证环境：Linux amd64、KVM、QEMU，Ubuntu 26.04.1 虚拟机。

### 功能

- **隔离：** 在硬件虚拟化的 Ubuntu 26.04 虚拟机里运行 AI Agent（QEMU/KVM），不挂载宿主机目录；项目打包送进 `/workspace`，跳过 `.env*`、`*.pem`、`id_*`、`.git/config` 和指向目录外的软链接。
- **网络策略：** `allowlist`（默认，只放行白名单域名，拒绝私网和云元数据地址，限制出站流量）、`offline`、`host`（直接用宿主机网络，显式开启、项目策略不能选）。项目策略只能收紧，不能放宽。默认白名单包含 Claude 登录用的域名。
- **取回成果：** `glasshouse pull` 先给 diff 再写入；宿主机自己解析 tar，拒绝绝对路径、`..`、硬链接和设备文件，去掉 setuid 位，不写穿软链接。
- **审计：** 宿主机上的哈希链审计日志，写不进去就拒绝联网和导出（失败即关闭）。
- **两种启动模式：** `--gui`（GNOME 桌面）和 `--shell`（轻量，只有终端）。
- **镜像：** `headless` 和 `desktop` 两种；`build.sh` 校验 Canonical 的签名，支持清华、中科大镜像源（`--mirror auto|tuna|ustc`），镜像写出时先写临时文件再原子改名。
- **预装软件：** 构建时安装最新版 Claude Code（支持订阅账号登录）、ccstatusline（沿用宿主机的配置）和固定提交的 CC-Monitor；CC-Monitor 的 hook 在安装时写入 Claude Code 的系统级托管设置，`agent` 用户改不了。
- **会话保留：** `--keep` 配合 `glasshouse start <会话>`，停止后再启动数据还在；监督进程消失的会话（`dead`）也能启动。
- **便捷脚本：** `start.sh`（保存桌面会话、一键启动）、`qemu-ssh`（一键进入 root shell）。
- **桌面：** 桌面会话默认不锁屏、不熄屏、不挂起（agent 没有密码，锁屏后无法解锁）；`glasshouse gui` 用一次性的 socat 桥让 `remote-viewer` 连上显示。
- **文档：** 中英文 README、使用手册，三张演示截图。

### 验证状态

**已验证：** 构建两种镜像、GNOME 桌面和自动登录、轻量 shell 和 `qemu-ssh`、保留磁盘的会话停止后再启动、订阅账号登录 Claude Code、`host` 网络模式联网、项目导入、`pull` 先审阅再写回、审计链校验、CC-Monitor 服务和 hook。

**未验证：** macOS（Apple 芯片）、arm64 镜像构建、CC-Monitor 在 Enforcing 模式下对真实 Claude Code 的拦截、经模型网关的 API Key 模式。

### 已知问题（在 V0.2 中修复）

- 项目导入偶发失败（`import failed: exit status 2`）：会话在 `/workspace` 创建之前就变成 `ready`。V0.1 标签说明里写的"项目导入"因此并不稳定。
