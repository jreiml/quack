package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func inviteLinkForTest(t *testing.T, s server, kind string) string {
	t.Helper()
	is := invites(s)
	for n := len(is) - 1; n >= 0; n-- {
		if is[n].Kind == kind {
			return s.get("addr") + "/" + is[n].ID
		}
	}
	t.Fatal("no invite", kind)
	return ""
}

func TestInviteAdmission(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := server{quack(t, "new", "-n", "t-invites", "--", "sleep", "300")}
	defer s.run("kill-server")
	i := createInvite(s, "pair", 1, time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("%064x", n+1)
			if requestAdmission(s, i.ID, "pair", id, "Ada Lovelace", "tiger-lamp") == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("%d admitted through one-use invite", successes)
	}
	consumed, _ := loadInvite(s, i.ID)
	if consumed.State != "consumed" {
		t.Fatal(consumed)
	}
	wrong := createInvite(s, "join", 0, 0)
	if err := requestAdmission(s, wrong.ID, "pair", strings.Repeat("f", 64), "Ada", "tiger-lamp"); err == nil {
		t.Fatal("terminal invite admitted a pair")
	}
	ask := createInvite(s, "pair", -1, 0)
	id := strings.Repeat("a", 64)
	if err := requestAdmission(s, ask.ID, "pair", id, "Ada", "tiger-lamp"); err != nil {
		t.Fatal(err)
	}
	if len(waiting(s)) != 1 {
		t.Fatal("missing waiter")
	}
	otherID := strings.Repeat("c", 64)
	if err := requestAdmission(s, ask.ID, "pair", otherID, "Grace", "tiger-lamp"); err != nil {
		t.Fatal(err)
	}
	n := 1
	changeInvite(s, ask.ID, &n, nil)
	if s.get("ok_"+id) != "Ada" || len(waiting(s)) != 0 {
		t.Fatal("policy change did not admit waiter")
	}
	if s.get("bye_"+otherID) == "" {
		t.Fatal("excess waiter was not told allowance was consumed")
	}
	revokeInvite(s, ask.ID, false, "revoked")
	if err := requestAdmission(s, ask.ID, "pair", id, "Ada", "tiger-lamp"); err == nil {
		t.Fatal("revoked invite accepted known peer")
	}
	expired := createInvite(s, "pair", 0, time.Hour)
	expired.Expires = time.Now().Add(-time.Second).Unix()
	expired.save(s)
	if err := requestAdmission(s, expired.ID, "pair", id, "Ada", "tiger-lamp"); err == nil {
		t.Fatal("expired invite admitted peer")
	}
}

func TestInviteRevocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := server{quack(t, "new", "-n", "t-revoke", "--", "sleep", "300")}
	defer s.run("kill-server")
	i := createInvite(s, "join", -1, 0)
	id := strings.Repeat("b", 64)
	if err := requestAdmission(s, i.ID, "join", id, "Ada", "tiger-lamp"); err != nil {
		t.Fatal(err)
	}
	admit(waiting(s)[0])
	s.set("guest_fake", id+"|Ada|tiger-lamp|12345")
	revokeInvite(s, i.ID, false, "revoked")
	if s.get("ok_"+id) == "" {
		t.Fatal("revoke disconnected active guest")
	}
	if err := requestAdmission(s, i.ID, "join", id, "Ada", "tiger-lamp"); err == nil {
		t.Fatal("revoke allowed reconnect")
	}
	s.unset("guest_fake")
	revokeInvite(s, i.ID, true, "ended")
	if s.get("ok_"+id) != "" {
		t.Fatal("approval left after disconnect")
	}
}

func TestNetInviteIsolation(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 for tailcat invites")
	}
	root := fakeSetup(t)
	s := server{quack(t, "new", "-n", "t-isolation", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
	defer s.run("kill-server")
	host := fakeInfo(t, root, "host")
	guest := startFake(t, root, "guest")
	quack(t, "share", "--auto-approve", "--limit", "1", s.name)
	terminalLink := inviteLinkForTest(t, s, "join")
	g := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "invite-isolation")}
	defer exec.Command(tmuxBin(), "-S", g.sock, "kill-server").Run()
	g.tmux("new-session", "-d", "-s", "host", "-x", "120", "-y", "30", bin+" attach "+s.name)
	eventually(t, "host attached", func() bool { return hostAttached(s) })
	g.join(terminalLink)
	eventually(t, "terminal admitted", func() bool { return attached(s) })
	g.tmux("send-keys", "-t", "host", "C-q")
	eventually(t, "host invite menu", func() bool { return strings.Contains(g.tmux("capture-pane", "-p", "-t", "host"), "Invite a Claude") })
	g.tmux("send-keys", "-t", "host", "c")
	eventually(t, "Claude invite chooser", func() bool {
		return strings.Contains(g.tmux("capture-pane", "-p", "-t", "host"), "allow one connection")
	})
	g.tmux("send-keys", "-t", "host", "1")
	eventually(t, "Claude command copied", func() bool {
		return strings.Contains(g.tmux("capture-pane", "-p", "-t", "host"), "Command copied") && len(invites(s)) == 2
	})
	pairLink := inviteLinkForTest(t, s, "pair")
	result := fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: pairLink})
	inbox := fakeInbox(t, guest, result.Output)
	eventually(t, "pair admitted", func() bool { return len(pairs(s)) == 1 && pairs(s)[0].state == "active" })
	_, id := splitInviteLink(pairLink)
	revokeInvite(s, id, false, "revoked")
	fakeCall(t, guest, fakeClaudeCommand{Action: "send", Address: inbox, Text: "still-paired-after-revoke"})
	eventually(t, "revoke preserves existing pair", func() bool { return hasFakeMessage(t, host, "still-paired-after-revoke") })
	rejected := fakeCallResult(t, guest, fakeClaudeCommand{Action: "pair", Address: pairLink})
	if !strings.Contains(rejected.Output, "revoked or expired") {
		eventually(t, "revoked pair rejected", func() bool { return hasFakeMessage(t, guest, "revoked or expired") })
	}
	stopAccess(s, "pair", "The host stopped agent messaging.")
	eventually(t, "agent access stopped", func() bool { return len(pairs(s)) == 0 && hasFakeMessage(t, guest, "stopped agent messaging") })
	if !attached(s) || !s.shared() {
		t.Fatal("stopping agents ended terminal sharing")
	}
	g.tmux("send-keys", "-t", "g", "C-q", "q")
	eventually(t, "terminal left", func() bool { return len(guests(s)) == 0 })
	g.join(terminalLink)
	eventually(t, "consumed terminal invite allows admitted peer to reconnect", func() bool { return attached(s) })
	_, terminalID := splitInviteLink(terminalLink)
	revokeInvite(s, terminalID, true, "The host revoked this terminal invite.")
	eventually(t, "terminal disconnected", func() bool {
		return len(guests(s)) == 0 && strings.Contains(g.screen(), "revoked this terminal invite")
	})
	g.join(terminalLink)
	eventually(t, "revoked terminal reconnect denied", func() bool { return strings.Contains(g.screen(), "revoked or expired") })
}

func TestInviteMenus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := server{quack(t, "new", "-n", "t-menu", "--", "sleep", "300")}
	defer s.run("kill-server")
	i := createInvite(s, "pair", -1, 0)
	g := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "invite-menus")}
	defer exec.Command(tmuxBin(), "-S", g.sock, "kill-server").Run()
	g.tmux("new-session", "-d", "-s", "g", "-x", "140", "-y", "45", bin+" attach "+s.name)
	eventually(t, "host attached", func() bool { return hostAttached(s) })
	keys := func(args ...string) { g.tmux(append([]string{"send-keys", "-t", "g"}, args...)...) }
	lastScreen := ""
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(lastScreen)
		}
	})
	shown := func(text string) {
		t.Helper()
		eventually(t, "menu: "+text, func() bool { lastScreen = g.screen(); return strings.Contains(lastScreen, text) })
	}
	keys("C-q")
	shown("Invite to terminal")
	keys("c")
	shown("Copy · allow one connection")
	keys("x")
	shown("Custom duration")
	keys("2")
	shown("Expiry: 2h")
	keys("Escape")
	shown("Manage access")
	keys("m")
	shown("Stop all agent messaging")
	keys("Home", "Enter")
	shown("Revoke and disconnect all")
	keys("a")
	shown("Allow next N")
	keys("n")
	shown("Number of admissions")
	keys("3", "Enter")
	eventually(t, "admission prompt applied", func() bool { v, _ := loadInvite(s, i.ID); return v.Remaining == 3 })
	shown("3 admissions left")
	keys("x")
	shown("Revoke this invite and disconnect everyone")
	keys("n")
	v, _ := loadInvite(s, i.ID)
	if v.State != "open" {
		t.Fatal("cancel revoked invite")
	}
	keys("C-q")
	shown("Manage access")
	keys("m")
	shown("Stop all agent messaging")
	keys("c")
	shown("Revoke Claude invites and disconnect all pairs")
	keys("y")
	eventually(t, "confirmed revocation", func() bool { v, _ := loadInvite(s, i.ID); return v.State == "revoked" })
}

func TestCloseAndInviteCleanup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := server{quack(t, "new", "-n", "t-invite-cleanup", "--", "sleep", "300")}
	defer s.run("kill-server")
	s.must("new-session", "-d", "-s", "_serve", "--", "sleep", "300")
	g := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "close-host")}
	defer exec.Command(tmuxBin(), "-S", g.sock, "kill-server").Run()
	g.tmux("new-session", "-d", "-s", "g", "-x", "120", "-y", "30", bin+" attach "+s.name)
	eventually(t, "host attached", func() bool { return hostAttached(s) })
	i := createInvite(s, "join", 1, time.Hour)
	id := strings.Repeat("d", 64)
	if err := requestAdmission(s, i.ID, "join", id, "Ada", "tiger-lamp"); err != nil {
		t.Fatal(err)
	}
	open := createInvite(s, "pair", 0, time.Hour)
	quack(t, "close", s.name)
	consumed, _ := loadInvite(s, i.ID)
	changed, _ := loadInvite(s, open.ID)
	if consumed.State != "consumed" || consumed.Admission != "limited" || consumed.Remaining != 0 {
		t.Fatal("close reopened consumed invite", consumed)
	}
	quack(t, "_expire", s.name)
	if !s.shared() {
		t.Fatal("idle cleanup stopped an open ask-first invite")
	}
	if changed.Admission != "ask" {
		t.Fatal("close left automatic admission", changed)
	}
	if openInviteCount(s) != 1 {
		t.Fatal("count includes consumed invites")
	}
	revokeInvite(s, open.ID, false, "revoked")
	expireInvites(s)
	if _, ok := loadInvite(s, open.ID); ok {
		t.Fatal("dead invite was retained")
	}
	if _, ok := loadInvite(s, i.ID); !ok {
		t.Fatal("consumed terminal invite needed for reconnect was removed")
	}
	if err := requestAdmission(s, i.ID, "join", id, "Ada", "tiger-lamp"); err != nil {
		t.Fatal("reconnect denied", err)
	}
	p := createInvite(s, "pair", 1, time.Hour)
	pairID := strings.Repeat("e", 64)
	if err := requestAdmission(s, p.ID, "pair", pairID, "Ada", "tiger-lamp"); err != nil {
		t.Fatal(err)
	}
	expireInvites(s)
	if _, ok := loadInvite(s, p.ID); ok {
		t.Fatal("unused consumed pair record retained")
	}
	if s.get("member_"+pairID) != "" || s.get("ok_"+pairID) != "" {
		t.Fatal("orphan pairing admission retained")
	}
	if openInviteCount(s) != 0 {
		t.Fatal("count includes dead invites")
	}
}

func TestNetIdleInviteExpiry(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 for tailcat invites")
	}
	t.Setenv("HOME", t.TempDir())
	s := server{quack(t, "new", "-n", "t-idle-expiry", "--", "sleep", "300")}
	defer s.run("kill-server")
	quack(t, "share", "--auto-approve", "--expires", "2s", s.name)
	eventually(t, "idle share stops after last invite expires", func() bool { return !s.shared() && s.get("addr") == "" })
	if !s.alive() {
		t.Fatal("expiry killed underlying session")
	}
	if len(invites(s)) != 0 {
		t.Fatal("expired invite records remain")
	}
}

func TestCrashedGateCleanup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := server{quack(t, "new", "-n", "t-crashed-gate", "--", "sleep", "300")}
	defer s.run("kill-server")
	i := createInvite(s, "join", -1, 0)
	id := strings.Repeat("f", 64)
	c := exec.Command("sleep", "300")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	start, err := processStart(c.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	s.set("gate_"+id, fmt.Sprintf("%d|%s", c.Process.Pid, start))
	if err := requestAdmission(s, i.ID, "join", id, "Ada", "tiger-lamp"); err != nil {
		t.Fatal(err)
	}
	if err := c.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err == nil {
		t.Fatal("killed gate succeeded")
	}
	revokeInvite(s, i.ID, false, "revoked")
	expireInvites(s)
	if _, ok := loadInvite(s, i.ID); ok {
		t.Fatal("crashed gate retained revoked invite")
	}
	for _, prefix := range []string{"gate_", "member_", "wait_", "ok_", "bye_"} {
		if s.get(prefix+id) != "" {
			t.Fatal("stale record", prefix)
		}
	}
}

func TestDetachedAskFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := server{quack(t, "new", "-n", "t-detached-ask", "--", "sleep", "300")}
	defer s.run("kill-server")
	for _, args := range [][]string{{"share", s.name}, {"close", s.name}} {
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "attached host") {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	s.must("new-session", "-d", "-s", "_serve", "--", "sleep", "300")
	createInvite(s, "join", -1, 0)
	quack(t, "_expire", s.name)
	if s.shared() {
		t.Fatal("unattended ask-first share stayed open")
	}
	if !s.alive() {
		t.Fatal("idle cleanup killed underlying session")
	}
}
