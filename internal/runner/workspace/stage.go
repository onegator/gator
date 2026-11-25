package workspace

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A checkpoint records the worktree without git ever reading a file of it: git would read the
// files through the repository's attributes and run whatever filter they name (from
// $GIT_DIR/info/attributes, a driver the agent configures while a checkpoint runs). The runner
// lists the files (git ls-files reads names, not contents), reads each through an os.Root of the
// worktree (no symlink, however it is swapped in, leads outside), writes the blobs itself, and
// hands git only the finished entries (update-index --index-info) to make a tree of.

// staged is what the runner knows of a file it hashed: enough to skip hashing it again.
type staged struct {
	size  int64
	mtime int64
	mode  fs.FileMode
	id    string
}

// stage builds the checkpoint's index from the worktree as it is now, into index.
func (w *Workspace) stage(ctx context.Context, g checkpointGit, index string, exclude []string) error {
	// The agent's index says what is tracked; it is only read.
	list := append([]string{"ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "."}, excludeSpecs(w.Dir, exclude)...)
	names, err := g.runIndex(ctx, filepath.Join(w.gitDir, "index"), list...)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	root, err := os.OpenRoot(w.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if w.format == "" {
		f, err := g.run(ctx, "", "rev-parse", "--show-object-format")
		if err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
		w.format = strings.TrimSpace(f)
	}
	if w.hashed == nil {
		w.hashed = map[string]staged{}
	}
	var entries bytes.Buffer
	seen := map[string]bool{}
	for _, name := range strings.Split(names, "\x00") {
		if name == "" || strings.HasSuffix(name, "/") || seen[name] {
			continue // a nested repository's directory, or a name listed twice
		}
		seen[name] = true
		mode, id, ok, err := w.blobOf(root, name)
		if err != nil {
			return err
		}
		if ok {
			fmt.Fprintf(&entries, "%s %s\t%s\x00", mode, id, name)
		}
	}
	_ = os.Remove(index)
	if err := os.MkdirAll(filepath.Dir(index), 0o700); err != nil {
		return err
	}
	if _, err := g.runInput(ctx, index, entries.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

// blobOf writes the blob of one file of the worktree and answers its tree mode and id; ok is false
// for a path that is gone or is no file (a directory, a device). Its contents are read inside the
// worktree only: a path through a symbolic link is refused by the os.Root.
func (w *Workspace) blobOf(root *os.Root, name string) (mode, id string, ok bool, err error) {
	info, err := root.Lstat(name)
	if err != nil {
		return "", "", false, nil // deleted, or reached through a link: not in the checkpoint
	}
	var data []byte
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := root.Readlink(name)
		if err != nil {
			return "", "", false, nil
		}
		mode, data = "120000", []byte(target) // a link is its target's path, never its contents
	case info.Mode().IsRegular():
		mode = "100644"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "100755"
		}
		if prior, ok := w.hashed[name]; ok && prior.size == info.Size() && prior.mtime == info.ModTime().UnixNano() && prior.mode == info.Mode() {
			return mode, prior.id, true, nil
		}
		if data, err = root.ReadFile(name); err != nil {
			return "", "", false, nil
		}
	default:
		return "", "", false, nil
	}
	if id, err = writeBlob(filepath.Join(w.bare, "objects"), w.format, data); err != nil {
		return "", "", false, err
	}
	if mode != "120000" {
		w.hashed[name] = staged{size: info.Size(), mtime: info.ModTime().UnixNano(), mode: info.Mode(), id: id}
	}
	return mode, id, true, nil
}

// writeBlob writes data as a loose blob into objects, as git would, and answers its id.
func writeBlob(objects, format string, data []byte) (string, error) {
	var h hash.Hash = sha1.New()
	if format == "sha256" {
		h = sha256.New()
	}
	header := fmt.Sprintf("blob %d\x00", len(data))
	h.Write([]byte(header))
	h.Write(data)
	id := hex.EncodeToString(h.Sum(nil))
	path := filepath.Join(objects, id[:2], id[2:])
	if _, err := os.Stat(path); err == nil {
		return id, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write([]byte(header))
	zw.Write(data)
	if err := zw.Close(); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "tmp_obj_")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(z.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	_ = os.Chmod(tmp.Name(), 0o444)
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return id, nil
}
