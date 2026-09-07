package report

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveReportOnlyRemovesSelectedFileAndAllowsRetry(t *testing.T) {
	store, _ := newFileStore(t)
	ctx := context.Background()
	for _, name := range []string{"tenant/one/report.html", "tenant/two/report.html"} {
		if _, err := store.Write(ctx, name, []byte("report")); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := store.Remove(ctx, "tenant/one/report.html"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.Open("tenant/one/report.html"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed file error=%v", err)
	}
	file, _, err := store.Open("tenant/two/report.html")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
}

func TestRemoveReportRejectsUnsafePathsDirectoriesAndCancellation(t *testing.T) {
	store, root := newFileStore(t)
	if _, err := store.Write(context.Background(), "tenant/report.html", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "../report.html", filepath.Join(root, "tenant/report.html"), "tenant"} {
		if err := store.Remove(context.Background(), name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Remove(ctx, "tenant/report.html"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tenant/report.html")); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveReportRejectsSymlink(t *testing.T) {
	store, root := newFileStore(t)
	if _, err := store.Write(context.Background(), "keep.html", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "keep.html"), filepath.Join(root, "link.html")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := store.Remove(context.Background(), "link.html"); err == nil {
		t.Fatal("accepted symlink")
	}
	if _, err := os.Stat(filepath.Join(root, "keep.html")); err != nil {
		t.Fatal(err)
	}
}
