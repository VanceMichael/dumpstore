package ansible

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTimeoutErrorWrapsSentinel(t *testing.T) {
	err := timeoutError("zfs_dataset_set.yml", 5*time.Minute)
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("errors.Is(err, ErrTimedOut) = false; err=%v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "zfs_dataset_set.yml") || !strings.Contains(msg, "5m0s") {
		t.Fatalf("error message lost context: %q", msg)
	}
}

// TestRunStreamingTimeoutClassified installs a fake ansible-playbook on PATH
// (a shell script that sleeps far beyond the runner timeout) and asserts the
// deadline is reported as ErrTimedOut. The test needs no real Ansible.
func TestRunStreamingTimeoutClassified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell helper required")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
sleep 2
`
	path := filepath.Join(dir, "ansible-playbook")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := &Runner{
		PlaybookDir:   dir,
		InventoryPath: filepath.Join(dir, "inventory"),
		Timeout:       50 * time.Millisecond,
		metrics:       *newAnsibleMetrics(),
	}
	_, err := r.RunStreaming(context.Background(), "anything.yml", nil, nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("errors.Is(err, ErrTimedOut) = false; err=%v", err)
	}
}
