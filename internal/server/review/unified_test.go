package review

import (
	"strings"
	"testing"
)

const twoFiles = `diff --git a/a.go b/a.go
index 1..2 100644
--- a/a.go
+++ b/a.go
@@ -1,3 +1,3 @@
 package a
-func  A() {}
+func A() {}
 // end
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -1,2 +1,3 @@
 package b
-var x = 1
+var x = 2
+var y = 3
diff --git a/logo.png b/logo.png
Binary files a/logo.png and b/logo.png differ
`

func TestIgnoreWhitespaceDropsWhatOnlyChangesSpace(t *testing.T) {
	got := string(IgnoreWhitespace([]byte(twoFiles)))
	if strings.Contains(got, "a.go") {
		t.Fatalf("a whitespace-only file is left:\n%s", got)
	}
	if !strings.Contains(got, "+var y = 3") || !strings.Contains(got, "-var x = 1") {
		t.Fatalf("a real change went:\n%s", got)
	}
	if !strings.Contains(got, "Binary files a/logo.png") {
		t.Fatalf("a binary change went:\n%s", got)
	}
	// A hunk with a real change keeps its whitespace pairs as context, so its header counts hold.
	mixed := "diff --git a/c.go b/c.go\n--- a/c.go\n+++ b/c.go\n@@ -1,2 +1,2 @@\n-x  =  1\n-y = 1\n+x = 1\n+y = 2\n"
	out := string(IgnoreWhitespace([]byte(mixed)))
	if !strings.Contains(out, "-x  =  1") || !strings.Contains(out, "+y = 2") {
		t.Fatalf("a pair run with a real change in it is kept whole:\n%s", out)
	}
	pairs := "diff --git a/d.go b/d.go\n--- a/d.go\n+++ b/d.go\n@@ -1,3 +1,3 @@\n-a  b\n+a b\n context\n-c\n+d\n"
	out = string(IgnoreWhitespace([]byte(pairs)))
	if !strings.Contains(out, "\n a b\n") || strings.Contains(out, "-a  b") || !strings.Contains(out, "+d") {
		t.Fatalf("pairs:\n%s", out)
	}
}

func TestComposePutsTurnsTogetherFileByFile(t *testing.T) {
	one := "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-1\n+2\ndiff --git a/b.go b/b.go\n@@ -1 +1 @@\n-x\n+y\n"
	two := "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-2\n+3\ndiff --git a/c.go b/c.go\n@@ -0,0 +1 @@\n+new\n"
	got := string(Compose([][]byte{[]byte(one), []byte(two)}))
	ia, ib, ic := strings.Index(got, "+2"), strings.Index(got, "+3"), strings.Index(got, "b/b.go")
	if ia < 0 || ib < ia || ic < ib || !strings.HasSuffix(got, "+new\n") {
		t.Fatalf("order:\n%s", got)
	}
	if strings.Count(got, "diff --git a/a.go") != 2 {
		t.Fatalf("each turn's part of a file is there:\n%s", got)
	}
}
