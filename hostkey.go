package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/crypto/ssh"
)

func hostKeyDirUsable() error {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(cfg, "tailcat", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	pem, err := os.ReadFile(filepath.Join(dir, "ssh_host_ed25519_key"))
	if err == nil {
		if _, err := ssh.ParsePrivateKey(pem); err != nil {
			return fmt.Errorf("parsing %s: %w", filepath.Join(dir, "ssh_host_ed25519_key"), err)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	probe, err := os.CreateTemp(dir, ".probe-")
	if err != nil {
		return err
	}
	probe.Close()
	return os.Remove(probe.Name())
}

func prepareHostKeyDir() (string, error) {
	err := hostKeyDirUsable()
	if err == nil {
		return "", nil
	}
	if runtime.GOOS == "darwin" {
		return "", fmt.Errorf("SSH host key: %w", err)
	}
	fallback := filepath.Join(os.TempDir(), "quack", "config")
	os.Setenv("XDG_CONFIG_HOME", fallback)
	if err2 := hostKeyDirUsable(); err2 != nil {
		return "", fmt.Errorf("SSH host key: %w; fallback: %w", err, err2)
	}
	return fmt.Sprintf("SSH host key: %v; using %s", err, fallback), nil
}
