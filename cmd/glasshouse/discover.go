package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"glasshouse/internal/session"
)

// Discovery for the control panel: which virtual machines exist on this machine. It looks in three places:
//   - the data directory this panel manages ($GLASSHOUSE_HOME), and the other data directories glasshouse may have been
//     used with (the default one, other users' defaults that are readable, and the ones running VMs point at);
//   - the running QEMU / Virtualization.framework processes, which also finds VMs that glasshouse did not start;
//   - session directories whose record cannot be read (orphans left by a crash or a manual delete).

const (
	vmSession  = "session"  // a session of the data directory this panel manages
	vmOther    = "other"    // a session of another glasshouse data directory
	vmExternal = "external" // a VM process that glasshouse did not start
	vmOrphan   = "orphan"   // a session directory without a readable record
)

type vmEntry struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	State      string `json:"state"`
	Home       string `json:"home,omitempty"`
	Path       string `json:"path,omitempty"`
	Backend    string `json:"backend,omitempty"`
	Profile    string `json:"profile,omitempty"`
	Project    string `json:"project,omitempty"`
	Persistent bool   `json:"persistent,omitempty"`
	PID        int    `json:"pid,omitempty"`
	CPUs       int    `json:"cpus,omitempty"`
	MemoryMiB  int    `json:"memory_mib,omitempty"`
	DiskBytes  int64  `json:"disk_bytes,omitempty"` // space the session's disk really occupies
	Cmd        string `json:"cmd,omitempty"`        // external VMs only: the start of the command line
}

type vmProc struct {
	PID       int
	Kind      string // qemu | vz
	Name      string
	Prog      string // the program's file name
	CPUs      int
	MemoryMiB int
	Home, ID  string // when the command line points into a glasshouse session directory
	Args      string
}

var sessionPathRE = regexp.MustCompile(`(/[^\s,=]*?)/sessions/(s-[0-9a-f]{6})(?:/|\s|,|$)`)

// psList is replaced in tests.
var psList = func() (string, error) {
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	return string(out), err
}

func isVMProgram(argv0 string) (kind string, ok bool) {
	b := filepath.Base(argv0)
	switch {
	case strings.HasPrefix(b, "qemu-system-"), b == "qemu-kvm", b == "qemu":
		return "qemu", true
	case b == "glasshouse-vz":
		return "vz", true
	}
	return "", false
}

func parseMiB(v string) int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "size=")
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(v, "G"), strings.HasSuffix(v, "g"):
		mult, v = 1024, v[:len(v)-1]
	case strings.HasSuffix(v, "M"), strings.HasSuffix(v, "m"):
		v = v[:len(v)-1]
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	return int(f * mult)
}

func parseCPUs(v string) int {
	v = strings.TrimPrefix(v, "cpus=")
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	n, _ := strconv.Atoi(v)
	return n
}

// parseProcesses turns `ps -axo pid=,args=` output into the VM processes in it.
func parseProcesses(out string) []vmProc {
	var procs []vmProc
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		sp := strings.IndexAny(line, " \t")
		if sp < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:sp])
		if err != nil {
			continue
		}
		args := strings.TrimSpace(line[sp:])
		f := strings.Fields(args)
		kind, ok := isVMProgram(f[0])
		if !ok {
			continue
		}
		p := vmProc{PID: pid, Kind: kind, Prog: filepath.Base(f[0]), Args: args}
		for i := 1; i < len(f); i++ {
			next := ""
			if i+1 < len(f) {
				next = f[i+1]
			}
			switch f[i] {
			case "-name":
				p.Name = strings.TrimPrefix(strings.SplitN(next, ",", 2)[0], "guest=")
			case "-smp":
				p.CPUs = parseCPUs(next)
			case "-m":
				p.MemoryMiB = parseMiB(next)
			}
		}
		if m := sessionPathRE.FindStringSubmatch(args); m != nil {
			p.Home, p.ID = m[1], m[2]
		}
		procs = append(procs, p)
	}
	return procs
}

// homeGlobs are the other users' default data directories; tests replace it.
var homeGlobs = []string{"/home/*/.local/share/glasshouse", "/Users/*/.local/share/glasshouse", "/root/.local/share/glasshouse"}

// candidateHomes lists the glasshouse data directories worth looking into.
func candidateHomes(current string, procs []vmProc) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		if h == "" {
			return
		}
		h = filepath.Clean(h)
		if r, err := filepath.EvalSymlinks(h); err == nil {
			h = r
		}
		if seen[h] {
			return
		}
		if st, err := os.Stat(filepath.Join(h, "sessions")); err != nil || !st.IsDir() {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	add(current)
	if u, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(u, ".local", "share", "glasshouse"))
	}
	for _, pat := range homeGlobs {
		m, _ := filepath.Glob(pat)
		sort.Strings(m)
		for _, h := range m {
			add(h)
		}
	}
	for _, p := range procs {
		add(p.Home)
	}
	return out
}

func diskUsage(dir string) int64 {
	m, _ := filepath.Glob(filepath.Join(dir, "disk.*"))
	var total int64
	for _, f := range m {
		var st syscall.Stat_t
		if syscall.Stat(f, &st) == nil {
			total += int64(st.Blocks) * 512
		}
	}
	return total
}

// discoverVMs returns every VM glasshouse can find on this machine, running ones first.
func discoverVMs(current string) (homes []string, vms []vmEntry) {
	out, _ := psList()
	procs := parseProcesses(out)
	homes = candidateHomes(current, procs)
	curReal := current
	if r, err := filepath.EvalSymlinks(current); err == nil {
		curReal = r
	}

	known := map[string]bool{} // home|id of every session directory found
	pidOf := map[string]vmProc{}
	for _, p := range procs {
		if p.ID != "" {
			pidOf[p.ID+"@"+p.Home] = p
		}
	}
	for _, h := range homes {
		kind := vmOther
		if h == curReal {
			kind = vmSession
		}
		ents, _ := os.ReadDir(session.SessionsDir(h))
		for _, e := range ents {
			if !e.IsDir() || !session.ValidID(e.Name()) {
				continue
			}
			dir := session.Dir(h, e.Name())
			known[h+"|"+e.Name()] = true
			in, err := session.Load(h, e.Name())
			if err != nil {
				vms = append(vms, vmEntry{Kind: vmOrphan, ID: e.Name(), State: "unknown", Home: h, Path: dir, DiskBytes: diskUsage(dir)})
				continue
			}
			state := in.State
			if (state == "ready" || state == "starting") && !in.Alive() {
				state = "dead"
			}
			v := vmEntry{Kind: kind, ID: in.ID, Name: "glasshouse-" + in.ID, State: state, Home: h, Path: dir, Backend: in.Backend, Profile: in.Profile,
				Project: in.Project, Persistent: in.Persistent, CPUs: in.CPUs, MemoryMiB: in.MemoryMiB, DiskBytes: diskUsage(dir)}
			if p, ok := pidOf[in.ID+"@"+h]; ok {
				v.PID = p.PID
			}
			vms = append(vms, v)
		}
	}
	for _, p := range procs {
		if p.ID != "" && known[p.Home+"|"+p.ID] {
			continue
		}
		cmd := p.Args
		if len(cmd) > 160 {
			cmd = cmd[:160] + "…"
		}
		name := p.Name
		if name == "" {
			name = p.Prog + " #" + strconv.Itoa(p.PID)
		}
		be := "qemu"
		if p.Kind == "vz" {
			be = "vz"
		}
		vms = append(vms, vmEntry{Kind: vmExternal, ID: name, Name: name, State: "running", PID: p.PID, Backend: be, CPUs: p.CPUs, MemoryMiB: p.MemoryMiB, Cmd: cmd})
	}
	rank := func(v vmEntry) int {
		switch {
		case v.State == "ready" || v.State == "running" || v.State == "starting":
			return 0
		case v.Kind == vmOrphan:
			return 3
		}
		return 1
	}
	sort.SliceStable(vms, func(i, j int) bool {
		if ri, rj := rank(vms[i]), rank(vms[j]); ri != rj {
			return ri < rj
		}
		return vms[i].ID > vms[j].ID
	})
	return homes, vms
}
