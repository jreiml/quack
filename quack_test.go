package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func TestCodeWords(t *testing.T) {
	if len(codeWords) != 256 {
		t.Fatalf("got %d code words, want 256", len(codeWords))
	}
	seen := map[string]bool{}
	for _, w := range codeWords {
		if seen[w] {
			t.Errorf("duplicate code word %q", w)
		}
		seen[w] = true
		if !regexp.MustCompile(`^[a-z]+$`).MatchString(w) {
			t.Errorf("code word %q is not plain lowercase", w)
		}
	}
}

func TestCodeFor(t *testing.T) {
	a := codeFor("nodekey:1111111111111111111111111111111111111111111111111111111111111111")
	if a != "salmon-cotton" {
		t.Errorf("codeFor changed: got %q", a)
	}
	if b := codeFor("nodekey:2222222222222222222222222222222222222222222222222222222222222222"); a == b {
		t.Errorf("different keys gave the same code %q", a)
	}
}

func TestNormalizeCode(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"tiger-lamp"}, "tiger-lamp"},
		{[]string{"Tiger", "Lamp"}, "tiger-lamp"},
		{[]string{" tiger  lamp "}, "tiger-lamp"},
		{[]string{"TIGER-", "-LAMP"}, "tiger-lamp"},
	} {
		if got := normalizeCode(tc.in); got != tc.want {
			t.Errorf("normalizeCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"Ada Lovelace":          "Ada Lovelace",
		"join":                  "join",
		"joiner":                "joiner",
		"":                      "someone",
		"Ré'my O.":              "Ré'my O.",
		"a|b#c$(rm -rf /)":      "abcrm -rf",
		strings.Repeat("x", 60): strings.Repeat("x", 40),
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLink(t *testing.T) {
	for _, in := range [][]string{
		{"tcABC"},
		{"Join my Claude session: quack join tcABC"},
		{"`tcABC`"},
	} {
		if got := parseLink(in); got != "tcABC" {
			t.Errorf("parseLink(%q) = %q", in, got)
		}
	}
}

func TestConnID(t *testing.T) {
	if got := connID("[fd7a:115c::1]:57205"); got != "_fd7a_115c_1_57205" {
		t.Errorf("connID = %q", got)
	}
}

func TestInputFilter(t *testing.T) {
	clock := time.Unix(1000, 0)
	f := &inputFilter{now: func() time.Time { return clock }}
	step := func(in string, want string) {
		t.Helper()
		if out := f.feed([]byte(in)); string(out) != want {
			t.Errorf("feed(%q) = %q; want %q", in, out, want)
		}
	}
	step("hello", "hello")
	step("\x03", "\x03")
	step("\x03", "")
	step("\x1b[99;5u", "")
	step("\x1b[27;5;99~", "")
	clock = clock.Add(4 * time.Second)
	step("\x1b[99;5u", "\x1b[99;5u")
	step("\x04", "")
	step("\x1b[100;5u", "")
	step("\x1b[99;5:3u", "")
	step("\x1b[99;6u", "\x1b[99;6u")
	step("\x11q", "\x11q")
	step("\x1b[113;5u", "\x1b[113;5u")
	step("\x1b[A", "\x1b[A")
	step("\x1b[57442;5u", "\x1b[57442;5u")
	step("\x1b[97;1:3u", "\x1b[97;1:3u")
}

var bin string

func TestMain(m *testing.M) {
	if os.Getenv("QUACK_CODEX_HELPER") == "1" || len(os.Args) > 1 && os.Args[1] == "queue" {
		fakeCodex()
		return
	}
	if os.Getenv("QUACK_PAIR_HELPER") == "1" {
		fakeClaude()
		return
	}
	dir, err := os.MkdirTemp("/tmp", "pk")
	if err != nil {
		panic(err)
	}
	bin = os.Getenv("QUACK_TEST_BIN")
	if bin == "" {
		bin = filepath.Join(dir, "quack")
		if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
			panic(string(out))
		}
	}
	os.Setenv("TMUX_TMPDIR", dir)
	os.Setenv("SSH_CONNECTION", "quack-test")
	os.Unsetenv("QUACK_SESSION")
	os.Unsetenv("TMUX")
	code := m.Run()
	for _, s := range servers() {
		s.run("kill-server")
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

func quack(t *testing.T, args ...string) string {
	t.Helper()
	if len(args) > 0 && (args[0] == "new" || args[0] == "claude" || args[0] == "codex") {
		args = append([]string{args[0], "--detach"}, args[1:]...)
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("quack %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSessions(t *testing.T) {
	name := quack(t, "new", "-n", "t-one", "--", "sleep", "300")
	if name != "t-one" {
		t.Fatalf("new printed %q", name)
	}
	s := server{name}
	if got := s.must("show-options", "-wv", "-t", "=main:", "window-size"); got != "manual" {
		t.Errorf("window-size = %q", got)
	}
	s.must("new-session", "-d", "-s", "_serve", "--", "sleep", "300")
	if got := s.must("show-options", "-gv", "status"); got != "on" {
		t.Errorf("status = %q", got)
	}
	if got := s.must("show-options", "-gv", "prefix"); got != "None" {
		t.Errorf("prefix = %q", got)
	}
	if got := s.must("show-environment", "-t", "=main", "QUACK_SESSION"); got != "QUACK_SESSION=t-one" {
		t.Errorf("env = %q", got)
	}
	if ls := quack(t, "ls"); !strings.Contains(ls, "t-one") || !strings.Contains(ls, "sleep 300") || !strings.Contains(ls, "detached") {
		t.Errorf("ls:\n%s", ls)
	}
	quack(t, "stop", "t-one")
	if s.alive() {
		t.Errorf("t-one still alive after stop")
	}
}

func TestSessionEndsWithCommand(t *testing.T) {
	s := server{quack(t, "new", "-n", "t-short", "--", "sleep", "1")}
	eventually(t, "session to end", func() bool { return !s.alive() })
	eventually(t, "tmux server to exit", func() bool { return s.cmd("list-sessions").Run() != nil })
}

func TestTerminalClose(t *testing.T) {
	outer := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "outer-close")}
	defer exec.Command(tmuxBin(), "-S", outer.sock, "kill-server").Run()
	host := func(s server) {
		outer.tmux("new-session", "-d", "-s", s.name, "-x", "100", "-y", "30", bin+" attach "+s.name+"; sleep 60")
		eventually(t, "host to attach", func() bool { return hostAttached(s) })
	}

	closed := server{quack(t, "new", "-n", "t-close", "--", "sleep", "300")}
	host(closed)
	outer.tmux("kill-session", "-t", closed.name)
	eventually(t, "closing the terminal to end the session", func() bool { return !closed.alive() })

	for _, away := range []bool{false, true} {
		name := "t-keep-attached"
		if away {
			name = "t-keep-away"
		}
		s := server{quack(t, "new", "-n", name, "--", "sleep", "300")}
		host(s)
		if away {
			createInvite(s, "join", 0, time.Hour)
		} else {
			quack(t, "detach", s.name)
			eventually(t, "host to detach", func() bool { return !hostAttached(s) })
		}
		outer.tmux("kill-session", "-t", s.name)
		time.Sleep(2 * time.Second)
		if !s.alive() {
			t.Errorf("away=%v: session ended", away)
		}
		s.run("kill-server")
	}
}

func TestStatusLine(t *testing.T) {
	s := server{quack(t, "new", "-n", "t-status", "--", "sleep", "300")}
	defer s.run("kill-server")
	if got := s.must("show-options", "-gv", "status-left"); strings.TrimSpace(got) != "🦆 Ctrl-Q" {
		t.Errorf("private status-left = %q", got)
	}
	s.set("wait_"+strings.Repeat("ab", 32), "tiger-lamp|Ada #1")
	refreshStatus(s)
	if got := s.must("show-options", "-gv", "status-left"); !strings.Contains(got, "🦆 Ctrl-Q") || !strings.Contains(got, "✋ Ada ##1 wants to join (code tiger-lamp)") {
		t.Errorf("status-left = %q", got)
	}
	s.unset("wait_" + strings.Repeat("ab", 32))
	refreshStatus(s)
	if got := s.must("show-options", "-gv", "status-left"); strings.TrimSpace(got) != "🦆 Ctrl-Q" {
		t.Errorf("status-left = %q with nobody around", got)
	}
}

func TestClientKeyPerLink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	take := func(addr string) (key.NodePrivate, *os.File) {
		k := clientKey(addr)
		return k, heldKey
	}
	a, fa := take("tcA")
	b, fb := take("tcB")
	c, fc := take("tcC")
	a2, fa2 := take("tcA")
	if a.Equal(b) || b.Equal(c) || a.Equal(a2) {
		t.Fatalf("links or parallel joins share a key")
	}
	for _, f := range []*os.File{fa, fb, fc, fa2} {
		f.Close()
	}
	for _, want := range []struct {
		addr string
		k    key.NodePrivate
	}{{"tcC", c}, {"tcA", a}, {"tcB", b}} {
		got, f := take(want.addr)
		if !got.Equal(want.k) {
			t.Errorf("reconnecting to %s got a different key", want.addr)
		}
		f.Close()
	}
}
