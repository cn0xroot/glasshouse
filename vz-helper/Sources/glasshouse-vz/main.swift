// glasshouse-vz: runs one Ubuntu VM on Apple's Virtualization.framework for Glasshouse.
//
// STATUS: compiles on Apple silicon (arm64, macOS 13+, Swift 5.9+) with
// `make vz-helper`. Booting a guest still needs an arm64 raw image (see README).
//
// Design (see docs): the VM has NO network device. The host talks to the guest
// only over vsock:
//   * sshSock   (host unix socket)  -> guest vsock port 22
//   * forwards  (guest vsock ports) -> host unix sockets (egress proxy, model gateway)
// so this process needs no network entitlement even when App-Sandboxed.

import AppKit
import Foundation
import Virtualization

struct Forward: Decodable { let vsockPort: UInt32; let unix: String }
struct Config: Decodable {
    let cpus: Int
    let memoryMiB: Int
    let disk: String
    let seed: String
    let gui: Bool
    let sshSock: String
    let ctrlSock: String
    let efiVars: String
    let readyFile: String
    let forwards: [Forward]
    // nat: a NAT network device, for image builds (apt/npm) and network.mode=host sessions only; every other session
    // has no NIC. extraDisks: read-only build inputs (image/build-mac.sh only).
    let nat: Bool?
    let extraDisks: [String]?
}

func die(_ msg: String) -> Never {
    FileHandle.standardError.write(Data(("glasshouse-vz: " + msg + "\n").utf8))
    exit(1)
}

// ---- args -----------------------------------------------------------------
let argv = CommandLine.arguments

// `glasshouse-vz check` reports what this Mac allows, for `glasshouse doctor` and the Go preflight.
if argv.count == 2, argv[1] == "check" {
    let mib: UInt64 = 1024 * 1024
    var arch = "unknown"
    #if arch(arm64)
    arch = "arm64"
    #elseif arch(x86_64)
    arch = "amd64"
    #endif
    let out: [String: Any] = [
        "supported": VZVirtualMachine.isSupported,
        "arch": arch,
        "minCPUs": VZVirtualMachineConfiguration.minimumAllowedCPUCount,
        "maxCPUs": VZVirtualMachineConfiguration.maximumAllowedCPUCount,
        "minMemoryMiB": VZVirtualMachineConfiguration.minimumAllowedMemorySize / mib,
        "maxMemoryMiB": VZVirtualMachineConfiguration.maximumAllowedMemorySize / mib,
    ]
    let data = try! JSONSerialization.data(withJSONObject: out, options: [.sortedKeys])
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write(Data("\n".utf8))
    exit(0)
}

guard argv.count == 4, argv[1] == "run", argv[2] == "--config" else {
    die("usage: glasshouse-vz run --config <file> | glasshouse-vz check")
}
guard VZVirtualMachine.isSupported else {
    die("Virtualization.framework is not available on this Mac (needs macOS 13+ with hardware virtualization; nested VMs are not supported)")
}

// A peer that closes its end must not kill the helper (and the VM with it): writes then fail with EPIPE instead.
signal(SIGPIPE, SIG_IGN)

// Every proxied request is one vsock connection plus one unix socket; the default limit (256) is far too low.
var nofile = rlimit()
if getrlimit(RLIMIT_NOFILE, &nofile) == 0 {
    let want = min(nofile.rlim_max, rlim_t(OPEN_MAX))
    if nofile.rlim_cur < want { nofile.rlim_cur = want; _ = setrlimit(RLIMIT_NOFILE, &nofile) }
}
guard let raw = FileManager.default.contents(atPath: argv[3]),
      let cfg = try? JSONDecoder().decode(Config.self, from: raw) else {
    die("cannot read config \(argv[3])")
}

// ---- helpers: byte pump between two file descriptors -----------------------
// Copies both directions until each side has sent EOF (or failed), then closes both descriptors and calls
// `done`, which releases whatever object owns the vsock connection. Nothing leaks per connection.
func pump(_ a: Int32, _ b: Int32, done: @escaping () -> Void = {}) {
    for fd in [a, b] {   // no SIGPIPE on these sockets even if a signal handler is changed later
        var one: Int32 = 1
        _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
    }
    func copy(from: Int32, to: Int32) {
        var buf = [UInt8](repeating: 0, count: 64 * 1024)
        outer: while true {
            let n = read(from, &buf, buf.count)
            if n < 0 && errno == EINTR { continue }
            if n <= 0 { break }
            var off = 0
            while off < n {
                let w = buf.withUnsafeBytes { write(to, $0.baseAddress! + off, n - off) }
                if w < 0 && errno == EINTR { continue }
                if w <= 0 { shutdown(from, SHUT_RD); break outer }
                off += w
            }
        }
        shutdown(to, SHUT_WR)
    }
    let group = DispatchGroup()
    let q = DispatchQueue.global()
    q.async(group: group) { copy(from: a, to: b) }
    q.async(group: group) { copy(from: b, to: a) }
    group.notify(queue: q) {
        close(a)
        close(b)
        done()
    }
}

// Keeps VZVirtioSocketConnection objects alive while their pump runs, and drops them afterwards.
final class ConnectionTable {
    private var items: [ObjectIdentifier: VZVirtioSocketConnection] = [:]
    private let lock = NSLock()
    func hold(_ c: VZVirtioSocketConnection) -> () -> Void {
        let key = ObjectIdentifier(c)
        lock.lock(); items[key] = c; lock.unlock()
        return { [weak self] in
            guard let self else { return }
            self.lock.lock(); let conn = self.items.removeValue(forKey: key); self.lock.unlock()
            DispatchQueue.main.async { conn?.close() }   // VZ objects belong to the VM's (main) queue
        }
    }
}
let connections = ConnectionTable()

// accept(2) that survives EINTR and backs off on persistent errors (EMFILE) instead of spinning a CPU.
func acceptLoop(_ fd: Int32, _ handle: @escaping (Int32) -> Void) {
    DispatchQueue.global().async {
        while true {
            let c = accept(fd, nil, nil)
            if c >= 0 { handle(c); continue }
            if errno == EINTR { continue }
            usleep(100_000)
        }
    }
}

func connectUnix(_ path: String) -> Int32? {
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    if fd < 0 { return nil }
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let bytes = Array(path.utf8CString)
    guard bytes.count <= MemoryLayout.size(ofValue: addr.sun_path) else { close(fd); return nil }
    withUnsafeMutableBytes(of: &addr.sun_path) { dst in
        for (i, c) in bytes.enumerated() { dst[i] = UInt8(bitPattern: c) }
    }
    let ok = withUnsafePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
        }
    }
    if ok != 0 { close(fd); return nil }
    return fd
}

func listenUnix(_ path: String) -> Int32 {
    unlink(path)
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let bytes = Array(path.utf8CString)
    guard fd >= 0, bytes.count <= MemoryLayout.size(ofValue: addr.sun_path) else { die("bad socket path \(path)") }
    withUnsafeMutableBytes(of: &addr.sun_path) { dst in
        for (i, c) in bytes.enumerated() { dst[i] = UInt8(bitPattern: c) }
    }
    let ok = withUnsafePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
        }
    }
    if ok != 0 || listen(fd, 16) != 0 { die("cannot listen on \(path)") }
    chmod(path, 0o600)
    return fd
}

// ---- VM configuration ------------------------------------------------------
func buildConfiguration() -> VZVirtualMachineConfiguration {
    let c = VZVirtualMachineConfiguration()
    let mib: UInt64 = 1024 * 1024
    c.cpuCount = min(max(cfg.cpus, VZVirtualMachineConfiguration.minimumAllowedCPUCount),
                     VZVirtualMachineConfiguration.maximumAllowedCPUCount)
    let mem = UInt64(max(cfg.memoryMiB, 1)) * mib
    c.memorySize = min(max(mem, VZVirtualMachineConfiguration.minimumAllowedMemorySize),
                       VZVirtualMachineConfiguration.maximumAllowedMemorySize) / mib * mib
    if c.cpuCount != cfg.cpus || c.memorySize != mem {
        FileHandle.standardError.write(Data("glasshouse-vz: clamped to \(c.cpuCount) vCPU / \(c.memorySize / mib) MiB (host limits)\n".utf8))
    }
    c.platform = VZGenericPlatformConfiguration()

    let boot = VZEFIBootLoader()
    let varsURL = URL(fileURLWithPath: cfg.efiVars)
    if FileManager.default.fileExists(atPath: cfg.efiVars) {
        boot.variableStore = VZEFIVariableStore(url: varsURL)
    } else {
        guard let store = try? VZEFIVariableStore(creatingVariableStoreAt: varsURL) else { die("cannot create EFI variable store") }
        boot.variableStore = store
    }
    c.bootLoader = boot

    // Root disk (raw image, APFS-cloned per session), the read-only cloud-init seed and any extra read-only disks.
    //
    // cachingMode MUST be .cached. With .automatic (the default of the plain initializer) Virtualization.framework can
    // read a sparse backing file around the macOS page cache while writes still sit in it, so under memory pressure the
    // guest reads back stale zeros for blocks it just wrote: silent ext4 corruption. Seen here as zeroed kernels, shims
    // and libraries (apt-get died with SIGILL executing a zeroed page); the same fix is in UTM (#5919), Lima and
    // apple/container. synchronizationMode .full flushes guest writes to storage, so a host crash or a forced stop
    // of a --keep session does not lose them.
    func attach(_ path: String, readOnly: Bool) -> VZDiskImageStorageDeviceAttachment {
        do {
            return try VZDiskImageStorageDeviceAttachment(url: URL(fileURLWithPath: path), readOnly: readOnly,
                                                          cachingMode: .cached, synchronizationMode: .full)
        } catch { die("cannot open disk image \(path): \(error)") }
    }
    c.storageDevices = [VZVirtioBlockDeviceConfiguration(attachment: attach(cfg.disk, readOnly: false)),
                        VZVirtioBlockDeviceConfiguration(attachment: attach(cfg.seed, readOnly: true))]
    for path in cfg.extraDisks ?? [] {
        c.storageDevices.append(VZVirtioBlockDeviceConfiguration(attachment: attach(path, readOnly: true)))
    }

    // Deliberately NO networkDevices, audio, USB passthrough or shared directories, except the NAT device that
    // an image build or a network.mode=host session asks for.
    if cfg.nat == true {
        let net = VZVirtioNetworkDeviceConfiguration()
        net.attachment = VZNATNetworkDeviceAttachment()
        c.networkDevices = [net]
    }
    c.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    c.socketDevices = [VZVirtioSocketDeviceConfiguration()]

    // Serial console -> console.log next to the sockets (host-only file).
    let consolePath = URL(fileURLWithPath: cfg.sshSock).deletingLastPathComponent().appendingPathComponent("console.log").path
    FileManager.default.createFile(atPath: consolePath, contents: nil, attributes: [.posixPermissions: 0o600])
    if let out = FileHandle(forWritingAtPath: consolePath) {
        out.seekToEndOfFile()
        let serial = VZVirtioConsoleDeviceSerialPortConfiguration()
        serial.attachment = VZFileHandleSerialPortAttachment(fileHandleForReading: nil, fileHandleForWriting: out)
        c.serialPorts = [serial]
    }

    if cfg.gui {
        let gpu = VZVirtioGraphicsDeviceConfiguration()
        gpu.scanouts = [VZVirtioGraphicsScanoutConfiguration(widthInPixels: 1440, heightInPixels: 900)]
        c.graphicsDevices = [gpu]
        c.keyboards = [VZUSBKeyboardConfiguration()]
        c.pointingDevices = [VZUSBScreenCoordinatePointingDeviceConfiguration()]
    }
    do { try c.validate() } catch { die("invalid VM configuration: \(error)") }
    return c
}

// ---- run -------------------------------------------------------------------
final class Delegate: NSObject, VZVirtualMachineDelegate {
    func guestDidStop(_ virtualMachine: VZVirtualMachine) { exit(0) }
    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        FileHandle.standardError.write(Data("vm stopped with error: \(error)\n".utf8))
        exit(1)
    }
}

final class ForwardListener: NSObject, VZVirtioSocketListenerDelegate {
    let unixPath: String
    init(_ p: String) { unixPath = p }
    func listener(_ listener: VZVirtioSocketListener, shouldAcceptNewConnection conn: VZVirtioSocketConnection,
                  from socketDevice: VZVirtioSocketDevice) -> Bool {
        guard let host = connectUnix(unixPath) else { return false }
        let guestFD = dup(conn.fileDescriptor)
        if guestFD < 0 { close(host); return false }
        pump(guestFD, host, done: connections.hold(conn))
        return true
    }
}

let vm = VZVirtualMachine(configuration: buildConfiguration())
let delegate = Delegate()
vm.delegate = delegate
var listeners: [ForwardListener] = []   // retained for the process lifetime
var window: NSWindow?

func showWindow() {
    guard cfg.gui else { return }
    DispatchQueue.main.async {
        if window == nil {
            let view = VZVirtualMachineView()
            view.virtualMachine = vm
            view.capturesSystemKeys = true
            let w = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1440, height: 900),
                             styleMask: [.titled, .closable, .miniaturizable, .resizable],
                             backing: .buffered, defer: false)
            w.title = "Glasshouse"
            w.contentView = view
            w.isReleasedWhenClosed = false
            w.center()
            window = w
        }
        window?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }
}

// Grace before a forced stop. Keep it well under the Go side's minimum (backend/vz: stopGrace), so the helper is never
// SIGKILLed first: a helper killed while its VM runs leaves the Virtualization XPC process (and the VM) behind.
let forcedStopAfter: Double = 10

var stopping = false
func stopVM() {
    DispatchQueue.main.async {
        if stopping { return }
        stopping = true
        // ACPI power button first: a clean shutdown. A guest stuck in firmware or a hung kernel ignores it.
        if vm.canRequestStop { try? vm.requestStop() }
        DispatchQueue.main.asyncAfter(deadline: .now() + forcedStopAfter) {
            guard vm.state != .stopped else { exit(0) }
            FileHandle.standardError.write(Data("glasshouse-vz: guest did not power off within \(Int(forcedStopAfter))s; forcing stop\n".utf8))
            vm.stop { err in
                if let err {
                    FileHandle.standardError.write(Data("glasshouse-vz: forced stop failed: \(err)\n".utf8))
                    exit(1)
                }
                exit(0)
            }
        }
    }
}

DispatchQueue.main.async {
    if cfg.gui { NSApplication.shared.setActivationPolicy(.regular) }
    vm.start { result in
        switch result {
        case .failure(let e): die("cannot start VM: \(e)")
        case .success:
            guard let dev = vm.socketDevices.first as? VZVirtioSocketDevice else { die("no vsock device") }
            for f in cfg.forwards {
                let l = ForwardListener(f.unix)
                listeners.append(l)
                let sl = VZVirtioSocketListener()
                sl.delegate = l
                dev.setSocketListener(sl, forPort: f.vsockPort)
            }
            // host unix socket -> guest sshd over vsock:22
            let sshFD = listenUnix(cfg.sshSock)
            acceptLoop(sshFD) { client in
                DispatchQueue.main.async {
                    dev.connect(toPort: 22) { res in
                        switch res {
                        case .success(let conn):
                            let g = dup(conn.fileDescriptor)
                            if g < 0 { close(client); conn.close(); return }
                            pump(client, g, done: connections.hold(conn))
                        case .failure: close(client)
                        }
                    }
                }
            }
            // control socket: "show" opens the window, "stop" powers off
            let ctrlFD = listenUnix(cfg.ctrlSock)
            acceptLoop(ctrlFD) { c in
                do {
                    var buf = [UInt8](repeating: 0, count: 64)
                    let n = read(c, &buf, buf.count)
                    close(c)
                    let cmd = n > 0 ? String(decoding: buf[0..<n], as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) : ""
                    if cmd == "show" { showWindow() } else if cmd == "stop" { stopVM() }
                }
            }
            FileManager.default.createFile(atPath: cfg.readyFile, contents: Data("ready\n".utf8), attributes: [.posixPermissions: 0o600])
            if cfg.gui { showWindow() }
        }
    }
}

signal(SIGTERM, SIG_IGN)
let term = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
term.setEventHandler { stopVM() }
term.resume()
signal(SIGINT, SIG_IGN)
let intr = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)
intr.setEventHandler { stopVM() }
intr.resume()

if cfg.gui { NSApplication.shared.run() } else { dispatchMain() }
