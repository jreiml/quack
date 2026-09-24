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
		"join Ada Lovelace":               "Ada Lovelace",
		"join":                            "someone",
		"":                                "someone",
		"join Ré'my O.":                   "Ré'my O.",
		"join a|b#c$(rm -rf /)":           "abcrm -rf",
		"join " + strings.Repeat("x", 60): strings.Repeat("x", 40),
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

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "pk")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "quack")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	os.Setenv("TMUX_TMPDIR", dir)
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
	if got := s.must("show-options", "-gv", "status"); got != "off" {
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
	if s.cmd("list-sessions").Run() == nil {
		t.Errorf("tmux server survived its command")
	}
}

func TestStatusLine(t *testing.T) {
	s := server{quack(t, "new", "-n", "t-status", "--", "sleep", "300")}
	defer s.run("kill-server")
	s.set("wait_"+strings.Repeat("ab", 32), "tiger-lamp|Ada #1")
	refreshStatus(s)
	if got := s.must("show-options", "-gv", "status"); got != "on" {
		t.Errorf("status = %q with a waiting guest", got)
	}
	if got := s.must("show-options", "-gv", "status-left"); !strings.Contains(got, "Ada ##1 is waiting") || !strings.Contains(got, "tiger-lamp · Ctrl-Q") {
		t.Errorf("status-left = %q", got)
	}
	s.unset("wait_" + strings.Repeat("ab", 32))
	refreshStatus(s)
	if got := s.must("show-options", "-gv", "status"); got != "off" {
		t.Errorf("status = %q with nobody around", got)
	}
}
