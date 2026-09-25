package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func isolateTemp(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("TMUX_TMPDIR"), "tmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
}

func TestAgentHostStore(t *testing.T) {
	root := fakeSetup(t)
	isolateTemp(t)
	fake := startFake(t, root, "store")
	h := newAgentHost(fake.Claude, nil)
	if !h.alive() || h.shared() || !h.attached() {
		t.Fatalf("new agent host: alive=%v shared=%v attached=%v", h.alive(), h.shared(), h.attached())
	}
	h.set("wait_abc", "tiger-lamp|Ada|pair")
	h.set("wait_def", "x|y")
	h.set("ok_abc", "Ada")
	if got := h.get("wait_abc"); got != "tiger-lamp|Ada|pair" {
		t.Fatalf("get = %q", got)
	}
	if got := h.opts("wait_"); len(got) != 2 || got["def"] != "x|y" {
		t.Fatalf("opts = %v", got)
	}
	h.unset("wait_def")
	h.unset("missing")
	if got := h.opts("wait_"); len(got) != 1 {
		t.Fatalf("opts after unset = %v", got)
	}
	if found, ok := hostByName(h.hostName()); !ok || found != host(h) {
		t.Fatalf("hostByName = %v %v", found, ok)
	}
	if got, ok := agentHostFor(fake.Claude, nil); !ok || got != h {
		t.Fatal("agentHostFor did not find the pinned agent")
	}
	if ls := quack(t, "ls"); !strings.Contains(ls, h.hostName()) || !strings.Contains(ls, "agent host") {
		t.Fatalf("ls:\n%s", ls)
	}
	if out, err := exec.Command(bin, "stop", h.hostName()).CombinedOutput(); err == nil || !strings.Contains(string(out), "not a terminal session") {
		t.Fatalf("stop on an agent host: %v %s", err, out)
	}
	fakeCall(t, fake, fakeClaudeCommand{Action: "exit"})
	eventually(t, "stale agent host swept", func() bool {
		agentHosts()
		_, err := os.Stat(h.dir())
		return os.IsNotExist(err)
	})
}

var inviteIDRx = regexp.MustCompile(`/([0-9a-f]{32})$`)

func TestInviteCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := server{quack(t, "new", "-n", "t-invite-cli", "--", "sleep", "300")}
	defer s.run("kill-server")
	s.must("new-session", "-d", "-s", "_serve", "--", "sleep", "300")
	s.set("addr", "test-addr")
	newInvite := func(args ...string) string {
		out := quack(t, append([]string{"invite", "new"}, args...)...)
		m := inviteIDRx.FindStringSubmatch(strings.SplitN(out, "\n", 2)[0])
		if m == nil {
			t.Fatalf("no invite link in %q", out)
		}
		return m[1]
	}
	term := newInvite("terminal", "--auto-approve", "-n", s.name)
	agent := newInvite("agent", "--auto-approve", "--limit", "2", "-n", s.name)
	if out := quack(t, "invite", "copy", term[:6]); !strings.Contains(out, "quack join test-addr/"+term) {
		t.Fatal(out)
	}
	if out := quack(t, "invite", "copy", agent[:6]); !strings.Contains(out, "! quack pair test-addr/"+agent) {
		t.Fatal(out)
	}
	ls := quack(t, "invite", "ls", "-n", s.name)
	for _, want := range []string{term[:6], agent[:6], "Terminal · anyone", "Agent · 2 admissions left"} {
		if !strings.Contains(ls, want) {
			t.Fatalf("invite ls missing %q:\n%s", want, ls)
		}
	}
	quack(t, "invite", "set", agent[:6], "--auto-approve", "--limit", "3", "--expires", "never")
	if i, _ := loadInvite(s, agent); i.Admission != "limited" || i.Remaining != 3 || i.Expires != 0 {
		t.Fatalf("set: %+v", i)
	}
	for _, args := range [][]string{
		{"invite", "copy", ""},
		{"invite", "copy", "zzzz"},
		{"invite", "new", "terminal", s.name},
		{"invite", "new", "window"},
		{"invite", "revoke", term[:6], "-n", s.name},
		{"invite", "revoke", "--all", term[:6]},
		{"invite", "set", term[:6]},
		{"invite", "set", term[:6], "--ask", "--auto-approve"},
		{"invite", "new", "terminal", "--limit", "1", "-n", s.name},
	} {
		if out, err := exec.Command(bin, args...).CombinedOutput(); err == nil {
			t.Fatalf("%v succeeded: %s", args, out)
		}
	}
	quack(t, "invite", "revoke", term[:6])
	if i, _ := loadInvite(s, term); i.State != "revoked" {
		t.Fatalf("revoke: %+v", i)
	}
	if out, err := exec.Command(bin, "invite", "copy", term[:6]).CombinedOutput(); err == nil {
		t.Fatalf("copied a revoked invite: %s", out)
	}
	quack(t, "invite", "revoke", "--all", "-n", s.name)
	if s.shared() || len(invites(s)) != 0 {
		t.Fatal("revoke --all left sharing on")
	}
}

func TestNetAgentHostPair(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 for tailcat pairing")
	}
	root := fakeSetup(t)
	isolateTemp(t)
	host := startFake(t, root, "host")
	guest := startFake(t, root, "guest")
	invite := func(args ...string) string {
		out := fakeCall(t, host, fakeClaudeCommand{Action: "quack", Args: append([]string{"invite", "new", "agent"}, args...)}).Output
		link, ok := strings.CutPrefix(strings.SplitN(out, "\n", 2)[0], "! quack pair ")
		if !ok {
			t.Fatalf("no pair command in %q", out)
		}
		return link
	}
	link := invite()
	hs := agentHosts()
	if len(hs) != 1 {
		t.Fatalf("agent hosts: %v", hs)
	}
	h := hs[0]
	if again := invite(); !strings.HasPrefix(again, strings.Split(link, "/")[0]+"/") || len(agentHosts()) != 1 {
		t.Fatal("second invite did not reuse the agent host")
	}
	result := fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: link})
	eventually(t, "pair waiting", func() bool { return len(allEntries(waiting)) == 1 })
	quack(t, "allow", allEntries(waiting)[0].code)
	hostInbox := fakeInbox(t, host, "")
	guestInbox := fakeInbox(t, guest, result.Output)
	fakeCall(t, guest, fakeClaudeCommand{Action: "send", Address: guestInbox, Text: "guest-to-host"})
	eventually(t, "guest message", func() bool { return hasFakeMessage(t, host, "guest-to-host") })
	fakeCall(t, host, fakeClaudeCommand{Action: "send", Address: hostInbox, Text: "host-to-guest"})
	eventually(t, "host message", func() bool { return hasFakeMessage(t, guest, "host-to-guest") })
	if ls := quack(t, "ls"); !strings.Contains(ls, h.hostName()) || !strings.Contains(ls, "1 pair") {
		t.Fatalf("ls:\n%s", ls)
	}
	if ls := quack(t, "invite", "ls", "-n", h.hostName()); !strings.Contains(ls, "Agent · ask first") {
		t.Fatalf("invite ls:\n%s", ls)
	}
	if out, err := exec.Command(bin, "invite", "new", "terminal", "-n", h.hostName()).CombinedOutput(); err == nil {
		t.Fatalf("terminal invite on an agent host: %s", out)
	}
	fakeCall(t, host, fakeClaudeCommand{Action: "exit"})
	eventually(t, "guest told", func() bool { return hasFakeMessage(t, guest, "Pairing ended") || hasFakeMessage(t, guest, "exited") })
	eventually(t, "agent host removed", func() bool { _, err := os.Stat(h.dir()); return os.IsNotExist(err) })
	path := strings.TrimPrefix(guestInbox, "uds:")
	eventually(t, "guest inbox removed", func() bool { _, err := os.Lstat(path); return os.IsNotExist(err) })
	if matches, _ := filepath.Glob(filepath.Join(agentHostsDir(), "*")); len(matches) != 0 {
		t.Fatalf("leftover agent hosts: %v", matches)
	}
}
