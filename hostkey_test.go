package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHostKeyDirFallsBackFromReadOnlyConfig(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("os.UserConfigDir ignores XDG_CONFIG_HOME on macOS")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(readOnly, 0o755) })
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", readOnly)
	t.Setenv("TMPDIR", tmp)
	notice, err := prepareHostKeyDir()
	if err != nil {
		t.Fatal(err)
	}
	if notice == "" {
		t.Fatal("expected a fallback notice")
	}
	want := filepath.Join(tmp, "quack", "config")
	if got := os.Getenv("XDG_CONFIG_HOME"); got != want {
		t.Fatalf("XDG_CONFIG_HOME = %q, want %q", got, want)
	}
	if err := hostKeyDirUsable(); err != nil {
		t.Fatal(err)
	}
}

func TestHostKeyDirRejectsMalformedKey(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("os.UserConfigDir ignores XDG_CONFIG_HOME on macOS")
	}
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "tailcat", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := hostKeyDirUsable(); err == nil {
		t.Fatal("expected an error for an empty host key")
	}
}
