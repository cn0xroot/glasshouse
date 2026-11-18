package vz

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// translated reports whether this process runs under Rosetta 2.
func translated() bool {
	v, err := syscall.SysctlUint32("sysctl.proc_translated")
	return err == nil && v == 1
}

// checkAPFS: PrepareDisk clones the base image with `cp -c`, which needs APFS (clonefile).
// It checks the nearest existing ancestor of dir, so it also works before the directory is created.
func checkAPFS(dir string) error {
	for d := dir; ; d = filepath.Dir(d) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(d, &st); err == nil {
			b := make([]byte, 0, len(st.Fstypename))
			for _, c := range st.Fstypename {
				if c == 0 {
					break
				}
				b = append(b, byte(c))
			}
			if name := string(b); name != "apfs" {
				return fmt.Errorf("%s is on a %s volume; sessions need APFS (copy-on-write clones). Set GLASSHOUSE_HOME to a directory on an APFS volume", dir, name)
			}
			return nil
		}
		if d == filepath.Dir(d) {
			return nil
		}
	}
}
