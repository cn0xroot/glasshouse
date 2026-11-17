// glasshouse-vz: runs one Ubuntu VM on Apple's Virtualization.framework for Glasshouse.
//
// STATUS: written without access to a Mac or the Swift toolchain, so it has
// NOT been compiled or run. Treat it as a reviewed starting point: build it
// with `make vz-helper`, fix any API drift, then run the checklist in README.
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
}

func die(_ msg: String) -> Never {
    FileHandle.standardError.write(Data(("glasshouse-vz: " + msg + "\n").utf8))
    exit(1)
}

// ---- args -----------------------------------------------------------------
let argv = CommandLine.arguments
guard argv.count == 4, argv[1] == "run", argv[2] == "--config" else {
    die("usage: glasshouse-vz run --config <file>")
}
guard let raw = FileManager.default.contents(atPath: argv[3]),
      let cfg = try? JSONDecoder().decode(Config.self, from: raw) else {
    die("cannot read config \(argv[3])")
}

// ---- helpers: byte pump between two file descriptors -----------------------
func pump(_ a: Int32, _ b: Int32) {
    func copy(from: Int32, to: Int32) {
        var buf = [UInt8](repeating: 0, count: 64 * 1024)
        while true {
            let n = read(from, &buf, buf.count)
            if n <= 0 { break }
            var off = 0
            while off < n {
                let w = buf.withUnsafeBytes { write(to, $0.baseAddress! + off, n - off) }
                if w <= 0 { return }
                off += w
            }
        }
        shutdown(to, SHUT_WR)
    }
    DispatchQueue.global().async { copy(from: a, to: b); }
    DispatchQueue.global().async { copy(from: b, to: a); }
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
    c.cpuCount = max(1, cfg.cpus)
    c.memorySize = UInt64(cfg.memoryMiB) * 1024 * 1024
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

    // Root disk (raw image, APFS-cloned per session) and the read-only cloud-init seed.
    guard let rootAtt = try? VZDiskImageStorageDeviceAttachment(url: URL(fileURLWithPath: cfg.disk), readOnly: false),
          let seedAtt = try? VZDiskImageStorageDeviceAttachment(url: URL(fileURLWithPath: cfg.seed), readOnly: true)
    else { die("cannot open disk images") }
    c.storageDevices = [VZVirtioBlockDeviceConfiguration(attachment: rootAtt),
                        VZVirtioBlockDeviceConfiguration(attachment: seedAtt)]

    // Deliberately NO networkDevices, audio, USB passthrough or shared directories.
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
        pump(guestFD, host)
        // keep `conn` alive for the life of the pump
        objc_setAssociatedObject(self, Unmanaged.passUnretained(conn).toOpaque(), conn, .OBJC_ASSOCIATION_RETAIN)
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

func stopVM() {
    DispatchQueue.main.async {
        if vm.canRequestStop { try? vm.requestStop() }  // ACPI power button: clean shutdown
        DispatchQueue.main.asyncAfter(deadline: .now() + 25) {
            vm.stop { _ in exit(0) }
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
            DispatchQueue.global().async {
                while true {
                    let client = accept(sshFD, nil, nil)
                    if client < 0 { continue }
                    DispatchQueue.main.async {
                        dev.connect(toPort: 22) { res in
                            switch res {
                            case .success(let conn):
                                let g = dup(conn.fileDescriptor)
                                pump(client, g)
                                objc_setAssociatedObject(dev, Unmanaged.passUnretained(conn).toOpaque(), conn, .OBJC_ASSOCIATION_RETAIN)
                            case .failure: close(client)
                            }
                        }
                    }
                }
            }
            // control socket: "show" opens the window, "stop" powers off
            let ctrlFD = listenUnix(cfg.ctrlSock)
            DispatchQueue.global().async {
                while true {
                    let c = accept(ctrlFD, nil, nil)
                    if c < 0 { continue }
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
