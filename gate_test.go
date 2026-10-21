package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGuestIDRoundTripsTTY(t *testing.T) {
	for _, tty := range []string{"/dev/pts/7", "/dev/ttys007"} {
		if got := guestTTY(guestID(tty)); got != tty {
			t.Errorf("guestTTY(guestID(%q)) = %q", tty, got)
		}
	}
}

func TestHasTerminfo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMINFO", dir)
	t.Setenv("TERMINFO_DIRS", "")
	if hasTerminfo("quack-test-term") || hasTerminfo("") || hasTerminfo("../x") {
		t.Fatal("found terminfo that doesn't exist")
	}
	os.MkdirAll(filepath.Join(dir, "q"), 0o700)
	os.WriteFile(filepath.Join(dir, "q", "quack-test-term"), nil, 0o600)
	if !hasTerminfo("quack-test-term") {
		t.Fatal("missed terminfo in $TERMINFO")
	}
}
