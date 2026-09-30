package claudesync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirectoryAcquisitionRejectsRedirectedAncestors(t *testing.T) {
	base, transcripts := t.TempDir(), t.TempDir()
	if err := os.Symlink(transcripts, filepath.Join(base, "redirect")); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		root, err := openDirectory(filepath.Join(base, "redirect", "new-backup"), create)
		if err == nil {
			root.Close()
			t.Fatal("followed an ancestor symlink")
		}
	}
	entries, err := os.ReadDir(transcripts)
	if err != nil || len(entries) != 0 {
		t.Fatal("directory creation escaped through a symlink")
	}
}

func TestOpenedDirectoryStaysPinnedAfterPathSwap(t *testing.T) {
	base, transcripts := t.TempDir(), t.TempDir()
	path := filepath.Join(base, "index")
	root, err := openDirectory(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(transcripts, path); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("local_new.json", []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path+"-original", "local_new.json")); err != nil {
		t.Fatal("root lost its directory handle")
	}
	entries, err := os.ReadDir(transcripts)
	if err != nil || len(entries) != 0 {
		t.Fatal("write escaped to transcripts")
	}
}

func TestConcurrentDesktopSyncRejectedBeforeLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Desktop lifecycle is macOS-only")
	}
	desktopSync.Lock()
	defer desktopSync.Unlock()
	if _, err := Run(context.Background(), nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy guard: %v", err)
	}
}
