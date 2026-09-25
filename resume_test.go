package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type testGate struct {
	cmd  *exec.Cmd
	wire *pairWire
}

func startTestGate(t *testing.T, s server, peerKey, inviteID string, hello pairFrame, env ...string) testGate {
	t.Helper()
	c := exec.Command(bin, "_gate", s.name)
	c.Env = append(append(os.Environ(), "TAILCAT_PEER_KEY="+peerKey, "SSH_ORIGINAL_COMMAND=pair-invite "+inviteID+" Ada Lovelace", "TAILCAT_REMOTE_ADDR=[::1]:"+peerKey[len(peerKey)-4:]), env...)
	c.Stderr = os.Stderr
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	g := testGate{c, newPairWire(out, in, in)}
	t.Cleanup(g.wire.close)
	if err := g.wire.send(hello); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g testGate) next(t *testing.T) pairFrame {
	t.Helper()
	select {
	case f := <-g.wire.in:
		return f
	case err := <-g.wire.errors:
		t.Fatalf("gate closed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("gate did not answer")
	}
	return pairFrame{}
}

func (g testGate) expect(t *testing.T, want string) pairFrame {
	t.Helper()
	for {
		if f := g.next(t); f.Type == want {
			return f
		} else if f.Type == "bye" {
			t.Fatalf("expected %s, got bye: %s", want, f.Text)
		}
	}
}

func (g testGate) kill(t *testing.T) {
	t.Helper()
	if err := g.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	g.cmd.Wait()
}

func countFakeMessages(t *testing.T, f fakeClaudeInfo, text string) int {
	n := 0
	for _, m := range fakeMessages(t, f) {
		if strings.Contains(m.Message.Content, text) {
			n++
		}
	}
	return n
}

func TestPairResumeGate(t *testing.T) {
	root := fakeSetup(t)
	s := server{quack(t, "new", "-n", "t-pair-resume", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
	defer s.run("kill-server")
	s.must("new-session", "-d", "-s", "_serve", "--", "sleep", "300")
	host := fakeInfo(t, root, "host")
	i := createInvite(s, "pair", 0, 0)
	key := "nodekey:" + strings.Repeat("ab", 32)
	me := agentIdentity{strings.Repeat("a", 64), "brave-otter-123456", "Ada Lovelace", "Codex"}
	hello := pairFrame{Type: "hello", Version: pairProtocol, Identity: &me}
	resume := hello
	resume.Resume = true

	first := startTestGate(t, s, key, i.ID, hello)
	first.expect(t, "ready")
	hostInbox := fakeInbox(t, host, "")
	first.wire.send(pairFrame{Type: "message", Seq: 1, Text: "guest-one"})
	if f := first.expect(t, "ack"); f.Seq != 1 {
		t.Fatalf("ack = %+v", f)
	}
	eventually(t, "guest message", func() bool { return hasFakeMessage(t, host, "guest-one") })
	fakeCall(t, host, fakeClaudeCommand{Action: "send", Address: hostInbox, Text: "host-one"})
	if f := first.expect(t, "message"); f.Seq != 1 || f.Text != "host-one" {
		t.Fatalf("message = %+v", f)
	}

	first.kill(t)
	eventually(t, "pairing offline", func() bool { ps := pairs(s); return len(ps) == 1 && ps[0].state == "offline" })
	eventually(t, "status shows reconnecting", func() bool {
		return strings.Contains(s.must("show-options", "-gv", "status-left"), "reconnecting")
	})
	fakeCall(t, host, fakeClaudeCommand{Action: "send", Address: hostInbox, Text: "host-two"})

	stranger := startTestGate(t, s, "nodekey:"+strings.Repeat("cd", 32), i.ID, resume)
	if f := stranger.next(t); f.Type != "bye" || !strings.Contains(f.Text, "ended while you were disconnected") {
		t.Fatalf("stranger resumed: %+v", f)
	}
	impostor := me
	impostor.Name = "other-otter-000000"
	wrong := hello
	wrong.Identity, wrong.Resume = &impostor, true
	if f := startTestGate(t, s, key, i.ID, wrong).next(t); f.Type != "bye" || !strings.Contains(f.Text, "another agent") {
		t.Fatalf("impostor resumed: %+v", f)
	}

	second := startTestGate(t, s, key, i.ID, resume)
	second.expect(t, "ready")
	got := map[int64]string{}
	for len(got) < 2 {
		f := second.expect(t, "message")
		got[f.Seq] = f.Text
	}
	if got[1] != "host-one" || got[2] != "host-two" {
		t.Fatalf("resent = %v", got)
	}
	second.wire.send(pairFrame{Type: "ack", Seq: 2})
	second.wire.send(pairFrame{Type: "message", Seq: 1, Text: "guest-one"})
	if f := second.expect(t, "ack"); f.Seq != 1 {
		t.Fatalf("duplicate ack = %+v", f)
	}
	second.wire.send(pairFrame{Type: "message", Seq: 2, Text: "guest-two"})
	second.expect(t, "ack")
	eventually(t, "second guest message", func() bool { return hasFakeMessage(t, host, "guest-two") })
	if n := countFakeMessages(t, host, "guest-one"); n != 1 {
		t.Fatalf("guest-one delivered %d times", n)
	}

	third := startTestGate(t, s, key, i.ID, resume)
	third.expect(t, "ready")
	select {
	case f := <-second.wire.in:
		t.Fatalf("old connection still used: %+v", f)
	case <-second.wire.errors:
	case <-time.After(5 * time.Second):
		t.Fatal("old connection was not closed")
	}
	revokeInvite(s, i.ID, false, "revoked")
	third.kill(t)
	fourth := startTestGate(t, s, key, i.ID, resume)
	fourth.expect(t, "ready")

	revokeInvite(s, i.ID, true, "The host revoked the invite.")
	if f := fourth.expect(t, "bye"); !strings.Contains(f.Text, "revoked") {
		t.Fatalf("bye = %+v", f)
	}
	eventually(t, "pairing removed", func() bool { return len(pairs(s)) == 0 })
	if f := startTestGate(t, s, key, i.ID, resume).next(t); f.Type != "bye" || !strings.Contains(f.Text, "revoked") {
		t.Fatalf("tombstone = %+v", f)
	}
	eventually(t, "host told", func() bool { return hasFakeMessage(t, host, "revoked") })
}

func TestPairOfflineLimit(t *testing.T) {
	root := fakeSetup(t)
	s := server{quack(t, "new", "-n", "t-pair-offline", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
	defer s.run("kill-server")
	s.must("new-session", "-d", "-s", "_serve", "--", "sleep", "300")
	host := fakeInfo(t, root, "host")
	i := createInvite(s, "pair", 0, 0)
	key := "nodekey:" + strings.Repeat("ef", 32)
	me := agentIdentity{strings.Repeat("b", 64), "calm-heron-123456", "Ada Lovelace", "Claude Code"}
	g := startTestGate(t, s, key, i.ID, pairFrame{Type: "hello", Version: pairProtocol, Identity: &me}, "QUACK_PAIR_OFFLINE_LIMIT=3s")
	g.expect(t, "ready")
	g.kill(t)
	eventually(t, "offline limit", func() bool { return len(pairs(s)) == 0 && hasFakeMessage(t, host, "connection to the peer was lost") })
}

func TestNetPairResume(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 for tailcat pairing")
	}
	root := fakeSetup(t)
	s := server{quack(t, "new", "-n", "t-pair-net-resume", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
	defer s.run("kill-server")
	host := fakeInfo(t, root, "host")
	guest := startFake(t, root, "guest")
	quack(t, "invite", "new", "agent", "--auto-approve", "-n", s.name)
	result := fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: inviteLinkForTest(t, s, "pair")})
	guestInbox := fakeInbox(t, guest, result.Output)
	hostInbox := fakeInbox(t, host, "")
	fakeCall(t, guest, fakeClaudeCommand{Action: "send", Address: guestInbox, Text: "before-drop"})
	eventually(t, "first message", func() bool { return hasFakeMessage(t, host, "before-drop") })
	for round := range 2 {
		var gates []int
		for _, v := range s.opts("pid_") {
			pid, err := strconv.Atoi(strings.TrimPrefix(v, "pair:"))
			if err != nil {
				t.Fatal(err)
			}
			if syscall.Kill(pid, 0) == nil {
				gates = append(gates, pid)
			}
		}
		if len(gates) != 1 {
			t.Fatalf("live gates: %v", gates)
		}
		if err := syscall.Kill(gates[0], syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		host2guest, guest2host := fmt.Sprintf("host-away-%d", round), fmt.Sprintf("guest-away-%d", round)
		fakeCall(t, host, fakeClaudeCommand{Action: "send", Address: hostInbox, Text: host2guest})
		fakeCall(t, guest, fakeClaudeCommand{Action: "send", Address: guestInbox, Text: guest2host})
		eventually(t, "messages across the reconnect", func() bool {
			return hasFakeMessage(t, guest, host2guest) && hasFakeMessage(t, host, guest2host)
		})
		eventually(t, "pairing active again", func() bool { ps := pairs(s); return len(ps) == 1 && ps[0].state == "active" })
		if countFakeMessages(t, guest, host2guest) != 1 || countFakeMessages(t, host, guest2host) != 1 {
			t.Fatal("message delivered more than once")
		}
	}
	for _, f := range []fakeClaudeInfo{host, guest} {
		if hasFakeMessage(t, f, "Pairing ended") || hasFakeMessage(t, f, "connection to the peer") {
			t.Fatal("reconnect announced an ending")
		}
	}
	fakeCall(t, guest, fakeClaudeCommand{Action: "unpair"})
	eventually(t, "unpair", func() bool { return len(pairs(s)) == 0 && hasFakeMessage(t, host, "ended the pairing") })
}

func stallingSSHServer(t *testing.T, conn net.Conn, exec bool) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	go func() {
		_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			if !exec {
				continue
			}
			ch, reqs, err := nc.Accept()
			if err != nil {
				return
			}
			defer ch.Close()
			go func() {
				for range reqs {
				}
			}()
		}
	}()
}

func TestPairSSHSetupBounded(t *testing.T) {
	cfg := pairConfig{Addr: "test", Invite: "abc", Identity: agentIdentity{Owner: "Ada"}}
	for _, exec := range []bool{false, true} {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("exec=%v/cancelled=%v", exec, cancelled), func(t *testing.T) {
				a, z := net.Pipe()
				defer z.Close()
				stallingSSHServer(t, z, exec)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				setup := time.Minute
				if cancelled {
					time.AfterFunc(300*time.Millisecond, cancel)
				} else {
					setup = 300 * time.Millisecond
				}
				done := make(chan error, 1)
				go func() {
					_, err := pairSSH(ctx, a, cfg, false, setup)
					done <- err
				}()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("setup succeeded against a stalled server")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("setup hung")
				}
			})
		}
	}
}

func TestPairLinkInitialAttachFails(t *testing.T) {
	a, z := net.Pipe()
	defer z.Close()
	var states []string
	dropped := 0
	link := &pairLink{b: &pairInbox{logger: log.New(io.Discard, "", 0)},
		attach:  func(pairConn) error { return errors.New("refused") },
		dropped: func() { dropped++ },
		state:   func(s string) { states = append(states, s) },
	}
	link.connect(pairConn{wire: newPairWire(a, a, a)})
	if link.offline.IsZero() || time.Since(link.offline) > time.Second || dropped != 1 || len(states) != 1 || states[0] != "offline" {
		t.Fatalf("offline=%v dropped=%d states=%v", link.offline, dropped, states)
	}
	first := link.offline
	link.connect(pairConn{wire: newPairWire(a, a, a)})
	if link.offline != first || dropped != 2 {
		t.Fatalf("second refusal reset the outage: offline=%v dropped=%d", link.offline, dropped)
	}
}

func TestPairWorkerLauncherGone(t *testing.T) {
	s := server{quack(t, "new", "-n", "t-pair-launcher", "--", "sleep", "300")}
	defer s.run("kill-server")
	i := createInvite(s, "pair", 0, 0)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	c := exec.Command(bin, "_pairhost", s.name, i.ID, "Ada Lovelace")
	c.Env = append(os.Environ(), "TAILCAT_PEER_KEY=nodekey:"+strings.Repeat("ab", 32))
	c.ExtraFiles = []*os.File{w}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		c.Process.Kill()
		t.Fatal("worker kept running without a launcher")
	}
	if v := s.get("pair_" + pairID("nodekey:"+strings.Repeat("ab", 32))); v != "" {
		t.Fatalf("worker record left behind: %q", v)
	}
	if _, err := os.Lstat(pairSocketPath(c.Process.Pid)); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestPairsSkipDeadWorkers(t *testing.T) {
	s := server{quack(t, "new", "-n", "t-pair-dead", "--", "sleep", "300")}
	defer s.run("kill-server")
	i := createInvite(s, "pair", 0, 0)
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pairSocketDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := pairSocketPath(c.Process.Pid)
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	defer os.Remove(stale)
	dead := pairID("nodekey:" + strings.Repeat("cd", 32))
	s.set("pair_"+dead, "Ada|tiger-lamp|"+strconv.Itoa(c.Process.Pid)+"|0|active")
	s.set("member_"+dead, i.ID)
	s.set("ok_"+dead, "Ada")
	if ps := pairs(s); len(ps) != 0 {
		t.Fatalf("pairs = %v", ps)
	}
	unlock := lockShare(s)
	pruneInvitesLocked(s)
	unlock()
	for _, opt := range []string{"pair_" + dead, "ok_" + dead, "member_" + dead} {
		if v := s.get(opt); v != "" {
			t.Fatalf("%s left behind: %q", opt, v)
		}
	}
	if s.get("invite_"+i.ID) == "" {
		t.Fatal("pruned the open invite")
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale socket left behind: %v", err)
	}
}

func TestPairRedialOutageIsCumulative(t *testing.T) {
	root := fakeSetup(t)
	fake := startFake(t, root, "redial")
	b, err := newPairInbox(fake.Claude, "Ada Lovelace", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	d := &pairDialer{logger: log.New(io.Discard, "", 0)}
	start := time.Now()
	if _, err := d.redial(context.Background(), b, false, time.Now().Add(-pairOfflineLimit()-time.Second)); err == nil || !strings.Contains(err.Error(), "lost the connection") {
		t.Fatalf("redial after the outage limit: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("redial kept trying past the outage limit")
	}
}

func TestPairInboundSlack(t *testing.T) {
	root := fakeSetup(t)
	fake := startFake(t, root, "slack")
	b, err := newPairInbox(fake.Claude, "Ada Lovelace", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	a, z := net.Pipe()
	defer z.Close()
	remote := newPairWire(z, z, z)
	defer remote.close()
	link := &pairLink{b: b, peer: "Ada Lovelace", wire: newPairWire(a, a, a), seen: map[string]bool{}}
	defer link.wire.close()
	for n := int64(1); n <= 61; n++ {
		if reason := link.receive(pairFrame{Type: "message", Seq: n, Text: fmt.Sprintf("msg-%d", n)}); reason != "" {
			t.Fatal(reason)
		}
		want := "ack"
		if n == 61 {
			want = "limit"
		}
		select {
		case f := <-remote.in:
			if f.Type != want {
				t.Fatalf("message %d: got %+v, want %s", n, f, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("message %d not answered", n)
		}
	}
}

func TestPairWireEscapedMessage(t *testing.T) {
	a, z := net.Pipe()
	local, remote := newPairWire(a, a, a), newPairWire(z, z, z)
	defer local.close()
	defer remote.close()
	text := strings.Repeat("<&>", pairMessageLimit/3)
	local.send(pairFrame{Type: "message", Seq: 1, Text: text})
	select {
	case f := <-remote.in:
		if f.Text != text {
			t.Fatal("message changed in transit")
		}
	case err := <-remote.errors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("message not delivered")
	}
}

func TestPairWaitingOffline(t *testing.T) {
	fakeSetup(t)
	s := server{quack(t, "new", "-n", "t-pair-wait-offline", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
	defer s.run("kill-server")
	s.must("new-session", "-d", "-s", "_serve", "--", "sleep", "300")
	i := createInvite(s, "pair", -1, 0)
	me := agentIdentity{strings.Repeat("c", 64), "keen-lynx-123456", "Ada Lovelace", "Codex"}
	hello := pairFrame{Type: "hello", Version: pairProtocol, Identity: &me}
	limit := "QUACK_PAIR_OFFLINE_LIMIT=6s"

	unapproved := "nodekey:" + strings.Repeat("12", 32)
	g := startTestGate(t, s, unapproved, i.ID, hello, limit)
	g.expect(t, "waiting")
	g.kill(t)
	eventually(t, "unapproved pairing ends at the offline limit", func() bool {
		return pairTombstone(s, pairID(unapproved)) == "The peer stopped waiting."
	})

	approved := "nodekey:" + strings.Repeat("34", 32)
	g = startTestGate(t, s, approved, i.ID, hello, limit)
	g.expect(t, "waiting")
	g.kill(t)
	dropped := time.Now()
	time.Sleep(3 * time.Second)
	quack(t, "allow", codeFor(approved))
	eventually(t, "approved pairing ends", func() bool { return pairTombstone(s, pairID(approved)) != "" })
	if reason := pairTombstone(s, pairID(approved)); reason != "The connection to the peer was lost." {
		t.Fatalf("ended with %q", reason)
	}
	if elapsed := time.Since(dropped); elapsed > 9*time.Second {
		t.Fatalf("approval restarted the outage: ended after %v", elapsed)
	}
}

func TestPairRedialBackoffPersists(t *testing.T) {
	d := &pairDialer{logger: log.New(io.Discard, "", 0)}
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		if _, err := d.redial(ctx, nil, false, time.Now()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		cancel()
		if d.delay != want {
			t.Fatalf("delay = %v, want %v", d.delay, want)
		}
	}
}

func TestPairSweepKeepsLiveSockets(t *testing.T) {
	isolateTemp(t)
	if err := os.MkdirAll(pairSocketDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	live := pairSocketPath(os.Getpid())
	ln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	sweepPairSockets()
	if _, err := os.Lstat(live); err != nil {
		t.Fatalf("swept a live process's socket: %v", err)
	}
}

func TestPairGateHelloTimeoutIsRetryable(t *testing.T) {
	fakeSetup(t)
	s := server{quack(t, "new", "-n", "t-pair-hello-timeout", "--", "sleep", "300")}
	defer s.run("kill-server")
	s.must("new-session", "-d", "-s", "_serve", "--", "sleep", "300")
	i := createInvite(s, "pair", 0, 0)
	key := "nodekey:" + strings.Repeat("56", 32)
	c := exec.Command(bin, "_gate", s.name)
	c.Env = append(os.Environ(), "TAILCAT_PEER_KEY="+key, "SSH_ORIGINAL_COMMAND=pair-invite "+i.ID+" Ada Lovelace", "TAILCAT_REMOTE_ADDR=[::1]:5656")
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	w := newPairWire(out, in, in)
	defer w.close()
	select {
	case f := <-w.in:
		t.Fatalf("gate answered a missing hello with %+v", f)
	case <-w.errors:
	case <-time.After(20 * time.Second):
		t.Fatal("gate kept the connection open")
	}
}
