// Package fsbridge moves project files across the VM boundary and treats
// everything coming back from the guest as hostile: the tar stream is parsed
// by the host, paths are confined, links are rejected, and nothing is
// written to the real project until the user has seen the diff.
package fsbridge

import (
	"archive/tar"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type Options struct {
	DenyPatterns []string // matched against the full relative path and the base name
	MaxBytes     int64    // total content bytes; 0 = unlimited
	MaxFiles     int      // 0 = unlimited
}

type Report struct {
	Files   int
	Bytes   int64
	Skipped []string // "path: reason"
}

// Denied reports whether rel (slash-separated, relative) matches a pattern.
// A pattern with a slash matches the whole path or any path suffix starting at
// a directory boundary (so ".git/config" also matches "sub/.git/config");
// a bare pattern matches any path element ("*.pem" matches "a/b/x.pem",
// ".env*" matches "app/.env.local", and "node_modules" matches the directory
// and everything under it).
func Denied(rel string, patterns []string) bool {
	rel = path.Clean(rel)
	parts := strings.Split(rel, "/")
	for _, pat := range patterns {
		if strings.Contains(pat, "/") {
			for i := range parts {
				if ok, _ := path.Match(pat, strings.Join(parts[i:], "/")); ok {
					return true
				}
			}
			// also match a prefix (a directory pattern like "secrets/*" hits deeper files)
			for i := 1; i <= len(parts); i++ {
				if ok, _ := path.Match(pat, strings.Join(parts[:i], "/")); ok {
					return true
				}
			}
			continue
		}
		for _, el := range parts {
			if ok, _ := path.Match(pat, el); ok {
				return true
			}
		}
	}
	return false
}

// Pack writes srcDir as a tar stream. Regular files and directories only;
// symlinks are kept only when they are relative and stay inside srcDir.
// Ownership is zeroed and setuid/setgid/sticky bits are stripped.
func Pack(srcDir string, w io.Writer, opt Options) (Report, error) {
	var rep Report
	root, err := filepath.Abs(srcDir)
	if err != nil {
		return rep, err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return rep, fmt.Errorf("%s is not a directory", srcDir)
	}
	tw := tar.NewWriter(w)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if Denied(rel, opt.DenyPatterns) {
			rep.Skipped = append(rep.Skipped, rel+": denied by pattern")
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: rel, ModTime: info.ModTime(), Mode: int64(info.Mode().Perm())}
		switch {
		case info.Mode().IsDir():
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			if opt.MaxFiles > 0 && rep.Files >= opt.MaxFiles {
				return fmt.Errorf("more than %d files", opt.MaxFiles)
			}
			if opt.MaxBytes > 0 && rep.Bytes+info.Size() > opt.MaxBytes {
				return fmt.Errorf("project exceeds the import limit of %d bytes", opt.MaxBytes)
			}
			hdr.Typeflag = tar.TypeReg
			hdr.Size = info.Size()
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			// Guard against a file that grows while we read it.
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			n, err := io.Copy(tw, io.LimitReader(f, info.Size()))
			if err != nil {
				return err
			}
			if n != info.Size() {
				return fmt.Errorf("%s changed while packing", rel)
			}
			rep.Files++
			rep.Bytes += n
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if !linkInside(rel, target) {
				rep.Skipped = append(rep.Skipped, rel+": symlink leaves the project")
				return nil
			}
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, target, 0o777
			return tw.WriteHeader(hdr)
		default:
			rep.Skipped = append(rep.Skipped, rel+": special file")
			return nil
		}
	})
	if err != nil {
		return rep, err
	}
	return rep, tw.Close()
}

// linkInside: relative target that, resolved from the link's directory, stays under the root.
func linkInside(rel, target string) bool {
	if target == "" || path.IsAbs(target) || filepath.IsAbs(target) {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(rel), filepath.ToSlash(target)))
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}

// Extract unpacks an untrusted tar stream into dstDir. dstDir must exist and
// should be an empty staging directory.
func Extract(r io.Reader, dstDir string, opt Options) (Report, error) {
	var rep Report
	root, err := filepath.Abs(dstDir)
	if err != nil {
		return rep, err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return rep, fmt.Errorf("corrupt archive: %w", err)
		}
		rel, err := cleanName(hdr.Name)
		if err != nil {
			return rep, fmt.Errorf("%q: %w", hdr.Name, err)
		}
		if rel == "." {
			continue
		}
		if Denied(rel, opt.DenyPatterns) {
			rep.Skipped = append(rep.Skipped, rel+": denied by pattern")
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return rep, err
			}
			continue
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := ensureParents(root, dst); err != nil {
			return rep, fmt.Errorf("%q: %w", rel, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(dst, 0o755); err != nil && !os.IsExist(err) {
				return rep, err
			}
			if st, err := os.Lstat(dst); err != nil || !st.IsDir() {
				return rep, fmt.Errorf("%q: not a directory", rel)
			}
		case tar.TypeReg:
			if opt.MaxFiles > 0 && rep.Files >= opt.MaxFiles {
				return rep, fmt.Errorf("archive has more than %d files", opt.MaxFiles)
			}
			if hdr.Size < 0 || (opt.MaxBytes > 0 && rep.Bytes+hdr.Size > opt.MaxBytes) {
				return rep, fmt.Errorf("archive exceeds the size limit of %d bytes", opt.MaxBytes)
			}
			mode := fs.FileMode(hdr.Mode).Perm() // drops setuid/setgid/sticky
			if mode&0o600 != 0o600 {
				mode |= 0o600
			}
			_ = os.Remove(dst)
			f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
			if err != nil {
				return rep, fmt.Errorf("%q: %w", rel, err)
			}
			n, err := io.Copy(f, io.LimitReader(tr, hdr.Size))
			cerr := f.Close()
			if err != nil || cerr != nil || n != hdr.Size {
				return rep, fmt.Errorf("%q: short write", rel)
			}
			_ = os.Chmod(dst, mode)
			rep.Files++
			rep.Bytes += n
		case tar.TypeSymlink:
			if !linkInside(rel, hdr.Linkname) {
				rep.Skipped = append(rep.Skipped, rel+": symlink leaves the project")
				continue
			}
			_ = os.Remove(dst)
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return rep, err
			}
		default:
			// hard links, devices, fifos, extended junk
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: unsupported entry type %q", rel, string(hdr.Typeflag)))
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

func cleanName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.Contains(name, "\\") {
		return "", errors.New("invalid path")
	}
	if strings.HasPrefix(name, "/") {
		return "", errors.New("absolute path")
	}
	c := path.Clean(name)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", errors.New("path escapes the destination")
	}
	return c, nil
}

// ensureParents makes sure every directory between root and dst's parent is a
// real directory inside root (never a symlink), creating missing ones.
func ensureParents(root, dst string) error {
	parent := filepath.Dir(dst)
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path escapes the destination")
	}
	cur := root
	if rel == "." {
		return nil
	}
	for _, el := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, el)
		st, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !st.IsDir() { // symlink or file in the way
			return fmt.Errorf("%s is not a plain directory", el)
		}
	}
	return nil
}

type ChangeKind string

const (
	Added    ChangeKind = "added"
	Modified ChangeKind = "modified"
	Deleted  ChangeKind = "deleted"
)

type Change struct {
	Path     string
	Kind     ChangeKind
	ModeOnly bool // content equal, permission bits differ
	ExecBit  bool // executable bit differs or a new executable file appears
}

// Diff compares the staged guest tree against the real project. Deleted files
// (present in project, absent in staging) are only reported when includeDeletes.
func Diff(projectDir, stagedDir string, deny []string, includeDeletes bool) ([]Change, error) {
	staged, err := index(stagedDir, deny)
	if err != nil {
		return nil, err
	}
	proj, err := index(projectDir, deny)
	if err != nil {
		return nil, err
	}
	var out []Change
	for p, s := range staged {
		o, ok := proj[p]
		switch {
		case !ok:
			out = append(out, Change{Path: p, Kind: Added, ExecBit: s.mode&0o111 != 0})
		case s.hash != o.hash:
			out = append(out, Change{Path: p, Kind: Modified, ExecBit: (s.mode^o.mode)&0o111 != 0})
		case s.mode != o.mode:
			out = append(out, Change{Path: p, Kind: Modified, ModeOnly: true, ExecBit: (s.mode^o.mode)&0o111 != 0})
		}
	}
	if includeDeletes {
		for p := range proj {
			if _, ok := staged[p]; !ok {
				out = append(out, Change{Path: p, Kind: Deleted})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type entry struct {
	hash string
	mode fs.FileMode
}

func index(dir string, deny []string) (map[string]entry, error) {
	m := map[string]entry{}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if Denied(rel, deny) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		m[rel] = entry{hash: fmt.Sprintf("%x", h.Sum(nil)), mode: info.Mode().Perm()}
		return nil
	})
	return m, err
}

// Apply copies the listed changes from stagedDir into projectDir. Destination
// parents are checked component by component so a symlink planted in the real
// project cannot redirect a write elsewhere.
func Apply(stagedDir, projectDir string, changes []Change) error {
	sroot, err := filepath.Abs(stagedDir)
	if err != nil {
		return err
	}
	proot, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}
	for _, c := range changes {
		rel, err := cleanName(c.Path)
		if err != nil {
			return fmt.Errorf("%q: %w", c.Path, err)
		}
		dst := filepath.Join(proot, filepath.FromSlash(rel))
		if err := ensureParents(proot, dst); err != nil {
			return fmt.Errorf("%q: %w", rel, err)
		}
		if c.Kind == Deleted {
			if st, err := os.Lstat(dst); err == nil && st.Mode().IsRegular() {
				if err := os.Remove(dst); err != nil {
					return err
				}
			}
			continue
		}
		src := filepath.Join(sroot, filepath.FromSlash(rel))
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		st, err := in.Stat()
		if err != nil {
			in.Close()
			return err
		}
		tmp := dst + ".glasshouse-tmp"
		_ = os.Remove(tmp)
		out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, st.Mode().Perm())
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return nil
}
