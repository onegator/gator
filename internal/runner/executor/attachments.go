package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

// imageTypes are the images an agent is shown as images (Claude's image blocks), by what their
// bytes say they are; maxImageBytes is the most one may be.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

const maxImageBytes = 5 << 20

// runnable are the names of files that run: they are handed as paths only, never as images, and
// never with the right to execute.
var runnable = []string{".sh", ".bash", ".zsh", ".command", ".exe", ".bat", ".cmd", ".ps1", ".app", ".dmg", ".pkg", ".jar",
	".bin", ".run", ".msi", ".deb", ".rpm", ".apk", ".py", ".pl", ".rb"}

// attachments fetches the files a job is handed, with its own identity, into a directory of the
// runner outside the worktree (they reach neither git nor a checkpoint), and answers them with a
// cleanup that removes them. A file that cannot be fetched is left out and said so; a job without
// an identity or files has none.
func (e *Executor) attachments(ctx context.Context, job proto.Job, emit func(string, any)) ([]backend.Attachment, func()) {
	none := func() {}
	if len(job.Attachments) == 0 || job.AgentToken == "" || e.ServerURL == "" {
		return nil, none
	}
	dir := filepath.Join(e.WS.Root, "attachments", filepath.Base(job.JobID))
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		emit("attachments_failed", map[string]any{"error": err.Error()})
		return nil, none
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	client := &http.Client{Timeout: 2 * time.Minute}
	var out []backend.Attachment
	for i, a := range job.Attachments {
		if i == proto.MaxAttachments {
			break
		}
		got, err := e.fetchAttachment(ctx, client, job, a, filepath.Join(dir, fmt.Sprintf("%02d-%s", i+1, safeName(a.Name))))
		if err != nil {
			emit("attachment_skipped", map[string]any{"name": a.Name, "error": err.Error()})
			continue
		}
		out = append(out, got)
	}
	emit("attachments", map[string]any{"files": len(out)})
	return out, cleanup
}

func (e *Executor) fetchAttachment(ctx context.Context, client *http.Client, job proto.Job, a proto.Attachment, path string) (backend.Attachment, error) {
	url := strings.TrimRight(e.ServerURL, "/") + proto.AttachmentPath + a.ID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return backend.Attachment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+job.AgentToken)
	res, err := client.Do(req)
	if err != nil {
		return backend.Attachment{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return backend.Attachment{}, fmt.Errorf("the server answered %s", res.Status)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return backend.Attachment{}, err
	}
	n, err := io.Copy(f, io.LimitReader(res.Body, proto.MaxAttachmentBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > proto.MaxAttachmentBytes {
		err = fmt.Errorf("over %d MB", proto.MaxAttachmentBytes>>20)
	}
	if err != nil {
		_ = os.Remove(path)
		return backend.Attachment{}, err
	}
	head := make([]byte, 512)
	if f, err := os.Open(path); err == nil {
		k, _ := io.ReadFull(f, head)
		head = head[:k]
		f.Close()
	}
	sniffed := http.DetectContentType(head)
	out := backend.Attachment{Path: path, Name: a.Name, MediaType: sniffed, Size: n}
	out.Image = imageTypes[sniffed] && n <= maxImageBytes && !runs(a.Name, head)
	return out, nil
}

// runs says whether a file is a program: by its name, or by what its first bytes say (a script,
// an ELF, Mach-O or PE binary).
func runs(name string, head []byte) bool {
	lower := strings.ToLower(name)
	for _, ext := range runnable {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	for _, magic := range [][]byte{[]byte("#!"), []byte("\x7fELF"), []byte("MZ"), {0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf},
		{0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
		if bytes.HasPrefix(head, magic) {
			return true
		}
	}
	return false
}

// safeName is a file's name as the runner keeps it: its base, in letters, digits, dot, dash and
// underscore, bounded.
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := strings.TrimLeft(b.String(), ".")
	if s == "" {
		s = "file"
	}
	if len(s) > 100 {
		s = s[len(s)-100:]
	}
	return s
}

// AttachmentsPrompt tells the agent where the files people sent are: outside the repository, to
// read, never to run.
func AttachmentsPrompt(files []backend.Attachment) string {
	var b strings.Builder
	b.WriteString("## Files people sent in the conversation\n\n")
	b.WriteString("They are outside the repository, for you to read (do not run them, and do not copy them into the repository unless asked):\n")
	for _, f := range files {
		what := f.MediaType
		if f.Image {
			what += ", shown to you as an image too"
		}
		fmt.Fprintf(&b, "\n- %s (%s, %d bytes): %s", f.Name, what, f.Size, f.Path)
	}
	return b.String()
}
