package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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

func attached(s server) bool {
	gs := guests(s)
	if len(gs) != 1 {
		return false
	}
	return strings.Contains(s.must("list-clients", "-t", "=main", "-F", "#{client_tty}"), gs[0].tty)
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
	addr := inviteLinkForTest(t, s, "join")
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
	if keys, _ := filepath.Glob(filepath.Join(home, ".config", "quack", "keys", "*.key")); len(keys) != 1 {
		t.Errorf("client keys saved: %v", keys)
	}

	quack(t, "allow", code)
	eventually(t, "guest to attach", func() bool { return attached(s) })
	g.tmux("send-keys", "-t", "g", "echo quack-$((6*7))", "Enter")
	eventually(t, "guest keystrokes to land", func() bool {
		return strings.Contains(s.must("capture-pane", "-p", "-t", "=main:"), "quack-42")
	})

	g.tmux("send-keys", "-t", "g", "C-q", "q")
	eventually(t, "guest to leave", func() bool { return len(guests(s)) == 0 })

	g.join(addr)
	eventually(t, "known guest to attach without approval", func() bool { return attached(s) })

	g.tmux("send-keys", "-t", "g", "C-q", "q")
	eventually(t, "guest to leave", func() bool { return len(guests(s)) == 0 })

	newKey := func() {
		if err := os.RemoveAll(filepath.Join(home, ".config", "quack", "keys")); err != nil {
			t.Fatal(err)
		}
	}
	newKey()
	for range 2 {
		g.join(addr)
		eventually(t, "new guest to wait", func() bool { return len(waiting(s)) == 1 && codeRx.MatchString(g.screen()) })
		quack(t, "decline", codeRx.FindStringSubmatch(g.screen())[1])
		eventually(t, "declined guest to see it", func() bool { return strings.Contains(g.screen(), "The host declined.") })
		if len(waiting(s)) != 0 {
			t.Errorf("declined guest still waiting")
		}
	}

	newKey()
	quack(t, "share", "--auto-approve", "--limit", "1", "t-net")
	addr = inviteLinkForTest(t, s, "join")
	g.join(addr)
	eventually(t, "guest to be auto-approved", func() bool { return attached(s) })
	_, consumedID := splitInviteLink(addr)
	consumed, _ := loadInvite(s, consumedID)
	if consumed.State != "consumed" {
		t.Errorf("auto-approve still on after its one use")
	}
	hostScreen := func() string { return g.tmux("capture-pane", "-p", "-t", "host") }
	eventually(t, "host status bar", func() bool {
		return strings.Contains(hostScreen(), "🌐 1 invite") && strings.Contains(hostScreen(), "👀 ")
	})
	eventually(t, "guest status bar", func() bool { return strings.Contains(g.screen(), "🦆 Ctrl-Q   🏠 ") })
	if strings.Contains(g.screen(), "Shared") || strings.Contains(g.screen(), "👀") {
		t.Errorf("guest sees the host's status bar:\n%s", g.screen())
	}
	quack(t, "detach", "t-net")
	eventually(t, "host to detach", func() bool {
		return len(strings.Fields(s.must("list-clients", "-t", "=main", "-F", "#{client_tty}"))) == 1
	})
	time.Sleep(time.Second)
	if !s.shared() {
		t.Fatalf("auto-approved share stopped when the host detached")
	}
	g.tmux("send-keys", "-t", "g", "C-q", "q")
	eventually(t, "guest to leave", func() bool { return len(guests(s)) == 0 })

	newKey()
	g.tmux("new-session", "-d", "-s", "host-again", "-x", "100", "-y", "30", bin+" attach "+s.name)
	eventually(t, "host reattached", func() bool { return hostAttached(s) })
	quack(t, "share", s.name)
	addr = inviteLinkForTest(t, s, "join")
	g.join(addr)
	eventually(t, "second guest to wait once the limit is used", func() bool { return len(waiting(s)) == 1 })
	quack(t, "unshare", "t-net")
	if s.shared() {
		t.Errorf("still shared after unshare")
	}
	eventually(t, "guest to be told", func() bool { return strings.Contains(g.screen(), "The host stopped sharing.") })
}

func TestNetTwoJoinsFromOneMachine(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 to run a real tailcat round trip")
	}
	t.Setenv("HOME", t.TempDir())
	s := server{quack(t, "new", "-n", "t-twice", "--", "bash", "--norc")}
	defer s.run("kill-server")
	quack(t, "share", "--auto-approve", "--limit", "1", "t-twice")
	addr := inviteLinkForTest(t, s, "join")
	g := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "guest2")}
	defer exec.Command(tmuxBin(), "-S", g.sock, "kill-server").Run()

	g.tmux("new-session", "-d", "-s", "g", "-x", "100", "-y", "30", bin+" join "+addr+"; sleep 120")
	eventually(t, "first join to be auto-approved", func() bool { return attached(s) })
	g.tmux("new-session", "-d", "-s", "host", "-x", "100", "-y", "30", bin+" attach "+s.name)
	eventually(t, "host attached", func() bool { return hostAttached(s) })
	quack(t, "share", s.name)
	addr = inviteLinkForTest(t, s, "join")
	g.tmux("new-session", "-d", "-s", "g2", "-x", "100", "-y", "30", bin+" join "+addr+"; sleep 120")
	eventually(t, "second join to wait", func() bool { return len(waiting(s)) == 1 })
	time.Sleep(30 * time.Second)
	if len(guests(s)) != 1 || len(waiting(s)) != 1 {
		t.Errorf("after 30s: %d guests, %d waiting\nfirst:\n%s\nsecond:\n%s", len(guests(s)), len(waiting(s)),
			g.tmux("capture-pane", "-p", "-t", "g"), g.tmux("capture-pane", "-p", "-t", "g2"))
	}
}
