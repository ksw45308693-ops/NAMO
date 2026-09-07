package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAPIKeyStorePersistsAndReloadsWithoutExposingInvalidInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := APIKeyStore{Path: filepath.Join(dir, "key")}
	if _, err := store.Read(); !errors.Is(err, ErrAPIKeyNotConfigured) {
		t.Fatalf("missing key: %v", err)
	}
	store.Fallback = "legacy-key"
	if key, err := store.Read(); err != nil || key != "legacy-key" {
		t.Fatalf("legacy fallback: %v", err)
	}
	for _, key := range []string{"first+/=key", "replacement-key"} {
		if err := store.Save("  " + key + "\r\n"); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Read(); err != nil || got != key {
			t.Fatalf("fresh saved key not used: %v", err)
		}
	}
	for _, invalid := range []string{"", "secret%2Bencoded", "two words", "key\nline", strings.Repeat("a", 4097)} {
		if err := store.Save(invalid); err == nil || (invalid != "" && strings.Contains(err.Error(), invalid)) {
			t.Fatal("invalid input accepted or exposed")
		}
		if got, _ := store.Read(); got != "replacement-key" {
			t.Fatal("invalid save replaced previous key")
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(store.Path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("key file must be owner-only")
		}
	}
	if err := os.WriteFile(store.Path, []byte("corrupt%2Bsecret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); err == nil || strings.Contains(err.Error(), "corrupt") {
		t.Fatal("corrupt file silently fell back or leaked")
	}
}

func TestAPIKeyStoreRejectsSharedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	store := APIKeyStore{Path: filepath.Join(dir, "key")}
	if err := store.Save("test-key"); err == nil {
		t.Fatal("saved into a shared directory")
	}
}

func TestAPIKeyStoreRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store := APIKeyStore{Path: filepath.Join(link, "key"), Fallback: "legacy"}
	if err := store.Save("test-key"); err == nil {
		t.Fatal("accepted symlink parent")
	}
	if _, err := store.Read(); err == nil {
		t.Fatal("read accepted symlink parent")
	}
}

func TestLoadedConfigCanReadLegacyAPIKeyWithoutSavedFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows default path regression")
	}
	t.Setenv("APPDATA", t.TempDir())
	cfg, err := Load(mapLookup(map[string]string{
		"DATABASE_URL": "postgres://localhost/namo", "SESSION_KEY": strings.Repeat("s", 32), "G2B_API_KEY": "legacy-key",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.G2BAPIKeyFile) {
		t.Fatal("default key path is not absolute on this OS")
	}
	if key, err := cfg.APIKeys().Read(); err != nil || key != "legacy-key" {
		t.Fatalf("legacy fallback rejected: %v", err)
	}
}
