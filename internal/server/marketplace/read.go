package marketplace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
)

// Rejection is a package of a registry that was not taken, and why.
type Rejection struct {
	Item   Item   `json:"item"`
	Reason string `json:"reason"`
}

// Contents are what one fetch of a registry yielded: the packages that verified, and those that
// did not. An index that does not verify yields nothing at all.
type Contents struct {
	Packages []Package
	Rejected []Rejection
}

// Read reads a registry snapshot: its index, checked against the signature, then every package
// it lists, each checked against its checksum and the limits and read by its kind. A package that
// fails is rejected on its own; the others are still good.
func Read(fsys fs.FS, v Verifier, kinds Kinds, lim Limits) (Contents, error) {
	var out Contents
	index, err := readLimited(fsys, IndexFile, MaxIndexBytes)
	if err != nil {
		return out, fmt.Errorf("%w: %s: %v", ErrInvalid, IndexFile, err)
	}
	sig, err := readLimited(fsys, SignatureFile, 4<<10)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return out, err
	}
	if err := v.VerifyIndex(index, sig); err != nil {
		return out, err
	}
	idx, err := ParseIndex(index, kinds.Names())
	if err != nil {
		return out, err
	}
	for _, it := range idx.Items {
		pkg, err := readPackage(fsys, it, v, kinds, lim)
		if err != nil {
			out.Rejected = append(out.Rejected, Rejection{Item: it, Reason: err.Error()})
			continue
		}
		out.Packages = append(out.Packages, pkg)
	}
	return out, nil
}

func readPackage(fsys fs.FS, it Item, v Verifier, kinds Kinds, lim Limits) (Package, error) {
	files, err := ReadFiles(fsys, it.Path, lim)
	if err != nil {
		return Package{}, fmt.Errorf("%s %s: %w", it.Key(), it.Version, err)
	}
	if err := v.VerifyPackage(it, files); err != nil {
		return Package{}, err
	}
	m, err := kinds.Describe(it, files)
	if err != nil {
		return Package{}, err
	}
	return Package{Item: it, Manifest: m, Files: files}, nil
}

func readLimited(fsys fs.FS, name string, max int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a plain file", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, max)
	}
	return b, nil
}
