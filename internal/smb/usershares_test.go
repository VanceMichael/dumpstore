package smb

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseUsersharePath(t *testing.T) {
	content := "#VERSION 0\npath=/tank/data/docs\ncomment=\nusershare_acl=S-1-1-0:F\nguest_ok=n\n"
	if got, ok := parseUsersharePath([]byte(content)); !ok || got != "/tank/data/docs" {
		t.Fatalf("parseUsersharePath = %q, %v", got, ok)
	}

	if _, ok := parseUsersharePath([]byte("#VERSION 0\ncomment=\nguest_ok=n\n")); ok {
		t.Fatal("expected no path to be reported as unparseable")
	}

	if _, ok := parseUsersharePath(nil); ok {
		t.Fatal("nil content must not yield a path")
	}
}

func TestListUsersharesMissingDir(t *testing.T) {
	// UsershareDir points at a system path on the dev host (macOS) where it
	// does not exist; this must be an empty, non-error result.
	if got := filepath.Base(UsershareDir(runtime.GOOS)); got != "usershares" {
		t.Fatalf("unexpected usershare dir tail: %q", got)
	}
	shares, err := ListUsershares(runtime.GOOS)
	if err != nil {
		t.Fatalf("missing usershare dir must not error: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("expected no shares, got %#v", shares)
	}
}

func TestListUsersharesDir(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("good", "#VERSION 0\npath=/tank/a\nusershare_acl=S-1-1-0:F\nguest_ok=n\n")
	write("other", "path=/tank/b\n")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(".hidden", "path=/tank/hidden\n")

	shares, err := listUsersharesDir(dir)
	if err != nil {
		t.Fatalf("listUsersharesDir: %v", err)
	}
	if len(shares) != 2 {
		t.Fatalf("expected 2 shares (dotfiles and dirs skipped), got %#v", shares)
	}
	got := map[string]string{}
	for _, s := range shares {
		got[s.Name] = s.Path
	}
	if got["good"] != "/tank/a" || got["other"] != "/tank/b" {
		t.Fatalf("unexpected shares: %#v", got)
	}
}

func TestListUsersharesMalformedFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken"), []byte("#VERSION 0\ncomment=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := listUsersharesDir(dir); err == nil {
		t.Fatal("malformed usershare file must produce an error so callers fail closed")
	}
}
