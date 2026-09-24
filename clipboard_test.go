package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClipboardTools(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux clipboard tools")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	t.Setenv("DISPLAY", ":test")
	t.Setenv("QUACK_CLIPBOARD_RESULT", filepath.Join(dir, "result"))
	for name, body := range map[string]string{
		"wl-copy": "exit 1",
		"xclip":   "exit 1",
		"xsel":    `/bin/cat > "$QUACK_CLIPBOARD_RESULT"`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := "! quack pair tc-example/0123456789abcdef0123456789abcdef"
	if !copyToClipboard(command) {
		t.Fatal("did not fall back to the working clipboard tool")
	}
	b, err := os.ReadFile(os.Getenv("QUACK_CLIPBOARD_RESULT"))
	if err != nil || string(b) != command {
		t.Fatalf("clipboard = %q, err = %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wl-copy"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if !copyToClipboard(command) {
		t.Fatal("a stalled clipboard tool prevented fallback")
	}
	t.Setenv("SSH_CONNECTION", "test")
	if copyToClipboard("remote") {
		t.Fatal("SSH session wrote to the server's desktop clipboard")
	}
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")
	if copyToClipboard("headless") {
		t.Fatal("reported a desktop clipboard without a display")
	}
}

func TestInviteClipboardFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SSH_CONNECTION", "test")
	s := server{quack(t, "new", "-n", "t-clipboard", "--", "sleep", "300")}
	defer s.run("kill-server")
	s.set("addr", "tc-clipboard-test")
	s.must("set-option", "-as", "terminal-features", ",tmux*:clipboard")
	i := createInvite(s, "pair", -1, 0)
	first := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "clipboard-first")}
	second := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "clipboard-second")}
	for _, g := range []guestTerm{first, second} {
		defer exec.Command(tmuxBin(), "-S", g.sock, "kill-server").Run()
		g.tmux("new-session", "-d", "-s", "g", "-x", "140", "-y", "35", bin+" attach "+s.name)
		g.tmux("set-option", "-s", "set-clipboard", "on")
	}
	eventually(t, "both terminals attached", func() bool {
		return len(strings.Fields(s.must("list-clients", "-F", "#{client_tty}"))) == 2
	})
	first.tmux("send-keys", "-t", "g", "C-q")
	eventually(t, "menu", func() bool { return strings.Contains(first.screen(), "Manage access") })
	first.tmux("send-keys", "-t", "g", "m")
	eventually(t, "invite list", func() bool { return strings.Contains(first.screen(), "Stop all agent messaging") })
	first.tmux("send-keys", "-t", "g", "1")
	eventually(t, "invite details", func() bool { return strings.Contains(first.screen(), "Show command") })
	first.tmux("send-keys", "-t", "g", "c")
	eventually(t, "manual copy fallback", func() bool {
		screen := first.screen()
		return strings.Contains(screen, i.command(s)) && strings.Contains(screen, "Press Enter to close")
	})
	if strings.Contains(first.screen(), "Command copied") {
		t.Fatal("claimed clipboard success without a desktop clipboard")
	}
	if got := s.must("show-buffer", "-b", "quack-invite"); got != i.command(s) {
		t.Fatalf("saved command = %q", got)
	}
	eventually(t, "clipboard escape delivered to requesting terminal", func() bool {
		out, err := exec.Command(tmuxBin(), "-S", first.sock, "show-buffer").CombinedOutput()
		return err == nil && strings.TrimSpace(string(out)) == i.command(s)
	})
	if out, err := exec.Command(tmuxBin(), "-S", second.sock, "show-buffer").CombinedOutput(); err == nil {
		t.Fatalf("copied to the other terminal: %s", out)
	}
	first.tmux("send-keys", "-t", "g", "Enter")
	eventually(t, "copy popup closed", func() bool { return !strings.Contains(first.screen(), "Press Enter to close") })
	first.tmux("set-option", "-s", "set-clipboard", "off")
	first.tmux("set-buffer", "old clipboard")
	first.tmux("send-keys", "-t", "g", "C-q")
	eventually(t, "menu without clipboard support", func() bool { return strings.Contains(first.screen(), "Manage access") })
	first.tmux("send-keys", "-t", "g", "m")
	eventually(t, "invite list without clipboard support", func() bool { return strings.Contains(first.screen(), "Stop all agent messaging") })
	first.tmux("send-keys", "-t", "g", "1")
	eventually(t, "details without clipboard support", func() bool { return strings.Contains(first.screen(), "Show command") })
	first.tmux("send-keys", "-t", "g", "c")
	eventually(t, "fallback without clipboard support", func() bool { return strings.Contains(first.screen(), i.command(s)) })
	if got := strings.TrimSpace(first.tmux("show-buffer")); got != "old clipboard" {
		t.Fatalf("clipboard changed despite disabled support: %q", got)
	}
	first.tmux("send-keys", "-t", "g", "Enter")
}
