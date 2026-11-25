package conversations

import (
	"context"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDiskStoreKeepsFilesWholeAndInside(t *testing.T) {
	ctx := context.Background()
	d := DiskStore{Root: t.TempDir()}
	if err := d.Put(ctx, "attachments/c/abc", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	r, err := d.Open(ctx, "attachments/c/abc")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "hello" {
		t.Fatalf("read back %q", b)
	}
	if err := d.Put(ctx, "attachments/c/short", strings.NewReader("hel"), 5, "text/plain"); err == nil {
		t.Fatal("a file that ended early was kept")
	}
	if _, err := d.Open(ctx, "attachments/c/short"); err == nil {
		t.Fatal("half a file is there")
	}
	for _, key := range []string{"../escape", "attachments/../../escape", ""} {
		if err := d.Put(ctx, key, strings.NewReader("x"), 1, ""); err == nil {
			t.Errorf("key %q was written", key)
		}
	}
	if err := d.Delete(ctx, "attachments/c/abc"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "attachments/c/abc"); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
}

func TestBlobStoreFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if s, err := BlobStoreFromEnv(env(nil)); s != nil || err != nil {
		t.Fatalf("nothing configured: %v %v", s, err)
	}
	s, err := BlobStoreFromEnv(env(map[string]string{"GATOR_ATTACHMENTS_DIR": "rel/dir"}))
	if d, ok := s.(DiskStore); err != nil || !ok || !strings.HasSuffix(d.Root, "rel/dir") || !strings.HasPrefix(d.Root, "/") {
		t.Fatalf("a directory: %#v %v", s, err)
	}
	s, err = BlobStoreFromEnv(env(map[string]string{"GATOR_ATTACHMENTS_DIR": "x", "GATOR_ATTACHMENTS_S3_ENDPOINT": "s3.example.com", "GATOR_ATTACHMENTS_S3_BUCKET": "b"}))
	if _, ok := s.(*S3Store); err != nil || !ok {
		t.Fatalf("a bucket wins over a directory: %#v %v", s, err)
	}
}

func TestCleanNameAndEmoji(t *testing.T) {
	for in, want := range map[string]string{"../../etc/passwd": "passwd", `C:\Users\me\plan.pdf`: "plan.pdf", "a\x00b\n.txt": "ab.txt", " spaced.png ": "spaced.png"} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
	for e, ok := range map[string]bool{"👍": true, ":ship-it:": true, "": false, "two words": false, "tab\t": false, strings.Repeat("x", 65): false} {
		if validEmoji(e) != ok {
			t.Errorf("validEmoji(%q) = %v", e, !ok)
		}
	}
}

// A long name is cut between characters, never inside one.
func TestCleanNameCutsWholeCharacters(t *testing.T) {
	name := strings.Repeat("ż", 200) + ".png" // 2 bytes each
	got := cleanName(name)
	if len(got) > maxAttachmentName || !utf8.ValidString(got) {
		t.Fatalf("%d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if got := cleanName("bad\xffbyte.txt"); !utf8.ValidString(got) || got != "badbyte.txt" {
		t.Fatalf("invalid UTF-8 kept: %q", got)
	}
}
