package marketplace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// File is one file of a package, at its path inside the package's folder.
type File struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

// Files are a package's files.
type Files []File

// Get answers the file at p.
func (ff Files) Get(p string) (File, bool) {
	for _, f := range ff {
		if f.Path == p {
			return f, true
		}
	}
	return File{}, false
}

// Under answers the files below dir, with their paths relative to it.
func (ff Files) Under(dir string) Files {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out Files
	for _, f := range ff {
		if rest, ok := strings.CutPrefix(f.Path, prefix); ok {
			out = append(out, File{Path: rest, Data: f.Data})
		}
	}
	return out
}

// Size is how many bytes the files hold.
func (ff Files) Size() int {
	n := 0
	for _, f := range ff {
		n += len(f.Data)
	}
	return n
}

// Checksum is the package's fingerprint: every file, in path order, as its path, its length and
// its bytes. The index carries the same value, so a file changed, added or removed after it was
// written is refused. The registry's tooling computes it the same way (see its README).
func (ff Files) Checksum() string {
	files := slices.Clone(ff)
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(len(f.Data))))
		h.Write([]byte{0})
		h.Write(f.Data)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Limits bound what one package may carry: a package is prompts and small scripts, and what is
// read from a registry ends up in the database and in every job that uses it.
type Limits struct {
	MaxFiles     int
	MaxFileBytes int
	MaxBytes     int // all files together
}

// DefaultLimits are the server's.
var DefaultLimits = Limits{MaxFiles: 200, MaxFileBytes: 1 << 20, MaxBytes: 4 << 20}

var segment = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9._-]*$`)

// SafePath says whether p may name a file inside a package or a package inside a registry:
// relative, clean, made of plain names, never climbing out.
func SafePath(p string) error {
	if p == "" || strings.ContainsAny(p, "\\\x00") || path.IsAbs(p) || path.Clean(p) != p {
		return invalid("path %q must be relative and clean", p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." || !segment.MatchString(seg) {
			return invalid("path %q must stay inside its folder and use plain names", p)
		}
	}
	return nil
}

// ReadFiles reads every file under dir of fsys within lim. A symbolic link, anything that is not
// a plain file or directory, an unsafe name or a file over the limits refuses the whole package:
// a link could point anywhere on the machine that fetched it.
func ReadFiles(fsys fs.FS, dir string, lim Limits) (Files, error) {
	if err := noLinksTo(fsys, dir); err != nil {
		return nil, err
	}
	var out Files
	total := 0
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel := p
		if dir != "." {
			rel = strings.TrimPrefix(p, dir+"/")
		}
		if err := SafePath(rel); err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return invalid("%s is a symbolic link", rel)
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return invalid("%s is not a plain file", rel)
		}
		if len(out) >= lim.MaxFiles {
			return invalid("more than %d files", lim.MaxFiles)
		}
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, int64(lim.MaxFileBytes)+1))
		if err != nil {
			return err
		}
		if len(data) > lim.MaxFileBytes {
			return invalid("%s is larger than %d bytes", rel, lim.MaxFileBytes)
		}
		if total += len(data); total > lim.MaxBytes {
			return invalid("the files are larger than %d bytes together", lim.MaxBytes)
		}
		out = append(out, File{Path: rel, Data: data})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, invalid("%s is not there", dir)
	}
	return out, err
}

// CheckFiles holds files that came from anywhere but a walk (the cache, a request) to the same
// rules ReadFiles does.
func CheckFiles(files Files, lim Limits) error {
	if len(files) > lim.MaxFiles {
		return invalid("more than %d files", lim.MaxFiles)
	}
	seen := map[string]bool{}
	total := 0
	for _, f := range files {
		if err := SafePath(f.Path); err != nil {
			return err
		}
		if seen[f.Path] {
			return invalid("%s twice", f.Path)
		}
		seen[f.Path] = true
		if len(f.Data) > lim.MaxFileBytes {
			return invalid("%s is larger than %d bytes", f.Path, lim.MaxFileBytes)
		}
		total += len(f.Data)
	}
	if total > lim.MaxBytes {
		return invalid("the files are larger than %d bytes together", lim.MaxBytes)
	}
	return nil
}

// noLinksTo refuses a package folder that is, or lies below, a symbolic link: WalkDir follows the
// folder it starts at, and a registry could point a package at any directory of the machine that
// fetched it. (Registries are read through os.Root as well, which keeps links inside.)
func noLinksTo(fsys fs.FS, dir string) error {
	if dir == "." {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		info, err := fs.Lstat(fsys, p)
		if errors.Is(err, errors.ErrUnsupported) {
			return nil
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return invalid("%s is not there", dir)
			}
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return invalid("%s is a symbolic link", p)
		}
	}
	return nil
}
