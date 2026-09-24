package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type guestTerm struct {
	t    *testing.T
	sock string
}

func (g guestTerm) tmux(args ...string) string {
	g.t.Helper()
	out, err := exec.Command(tmuxBin(), append([]string{"-S", g.sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
	if err != nil {
		g.t.Fatalf("guest tmux %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (g guestTerm) join(addr string) {
	exec.Command(tmuxBin(), "-S", g.sock, "kill-session", "-t", "g").Run()
	g.tmux("new-session", "-d", "-s", "g", "-x", "100", "-y", "30", bin+" join "+addr+"; sleep 120")
}

func (g guestTerm) screen() string { return g.tmux("capture-pane", "-p", "-t", "g") }

var codeRx = regexp.MustCompile(`Send them this code:\s+(\S+)`)

func TestNetShareJoin(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 to run a real tailcat round trip")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := server{quack(t, "new", "-n", "t-net", "-s", "--", "bash", "--norc")}
	defer s.run("kill-server")
	addr := s.get("addr")
	if !strings.HasPrefix(addr, "tc") {
		t.Fatalf("addr = %q", addr)
	}
	g := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "guest")}
	defer exec.Command(tmuxBin(), "-S", g.sock, "kill-server").Run()

	g.tmux("new-session", "-d", "-s", "host", "-x", "100", "-y", "30", bin+" attach t-net")
	eventually(t, "host to attach", func() bool { return strings.TrimSpace(s.must("list-clients", "-t", "=main")) != "" })

	g.join(addr)
	eventually(t, "guest to wait", func() bool { return len(waiting(s)) == 1 && codeRx.MatchString(g.screen()) })
	code := codeRx.FindStringSubmatch(g.screen())[1]
	if w := waiting(s)[0]; w.code != code {
		t.Fatalf("host sees code %q, guest shows %q", w.code, code)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "quack", "client.key")); err != nil {
		t.Errorf("client key not saved: %v", err)
	}

	quack(t, "allow", code)
	eventually(t, "guest to attach", func() bool { return len(guests(s)) == 1 })
	g.tmux("send-keys", "-t", "g", "echo quack-$((6*7))", "Enter")
	eventually(t, "guest keystrokes to land", func() bool {
		return strings.Contains(s.must("capture-pane", "-p", "-t", "=main:"), "quack-42")
	})

	g.tmux("send-keys", "-t", "g", "C-q", "q")
	eventually(t, "guest to leave", func() bool { return len(guests(s)) == 0 })

	g.join(addr)
	eventually(t, "known guest to attach without approval", func() bool { return len(guests(s)) == 1 })

	quack(t, "kick")
	eventually(t, "kicked guest to leave", func() bool { return len(guests(s)) == 0 })
	g.join(addr)
	eventually(t, "kicked guest to wait again", func() bool { return len(waiting(s)) == 1 })

	quack(t, "kick")
	eventually(t, "declined guest to see it", func() bool { return strings.Contains(g.screen(), "declined") })
	if len(waiting(s)) != 0 {
		t.Errorf("declined guest still waiting")
	}

	g.join(addr)
	eventually(t, "guest to wait", func() bool { return len(waiting(s)) == 1 })
	quack(t, "unshare", "t-net")
	if s.shared() {
		t.Errorf("still shared after unshare")
	}
	eventually(t, "guest to be told", func() bool { return strings.Contains(g.screen(), "The host stopped sharing.") })

}
