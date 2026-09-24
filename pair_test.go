package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPairRate(t *testing.T) {
	var r pairRate
	now := time.Unix(1000, 0)
	for range 30 {
		if !r.take(now) {
			t.Fatal("refused within limit")
		}
	}
	if r.take(now.Add(10*time.Minute - time.Nanosecond)) {
		t.Fatal("allowed over limit")
	}
	if !r.take(now.Add(10 * time.Minute)) {
		t.Fatal("did not release expired messages")
	}
}

func TestPairWireBounds(t *testing.T) {
	for _, input := range []string{"not json\n", `{"type":"message","text":"` + strings.Repeat("x", pairMessageLimit+1) + "\"}\n", strings.Repeat("x", 3*pairMessageLimit) + "\n"} {
		p := newPairWire(strings.NewReader(input), io.Discard)
		select {
		case <-p.errors:
		case <-time.After(time.Second):
			t.Fatal("invalid frame accepted or blocked")
		}
		close(p.done)
	}
}

func TestMessageBody(t *testing.T) {
	body := "A message\nwith <markup> and \"quotes\"."
	wrapped := "<cross-session-message from=\"uds:/tmp/a.sock\" from-mode=\"bypass\">\n" + body + "\n</cross-session-message>"
	if got := messageBody(wrapped); got != body {
		t.Fatalf("body = %q", got)
	}
	if got := messageBody(body); got != body {
		t.Fatalf("plain body changed: %q", got)
	}
}

type fakeClaudeInfo struct {
	Claude  claudeEndpoint `json:"claude"`
	Control string         `json:"control"`
}

type fakeClaudeCommand struct {
	Action  string `json:"action"`
	Address string `json:"address,omitempty"`
	Text    string `json:"text,omitempty"`
	ID      string `json:"id,omitempty"`
}

type fakeClaudeResult struct {
	Output   string       `json:"output,omitempty"`
	Error    string       `json:"error,omitempty"`
	Messages []localFrame `json:"messages,omitempty"`
}

func fakeClaude() {
	root, label := os.Getenv("QUACK_PAIR_ROOT"), os.Getenv("QUACK_PAIR_LABEL")
	if err := os.MkdirAll(claudeSessions(), 0o700); err != nil {
		panic(err)
	}
	dir := filepath.Join(root, "cc-socks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	pid := os.Getpid()
	path := filepath.Join(dir, strconv.Itoa(pid)+".sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		panic(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		panic(err)
	}
	start, err := processStart(pid)
	if err != nil {
		panic(err)
	}
	a := claudeEndpoint{pid, path, start}
	keyPath := claudeKeyPath(pid, path)
	token := strings.Repeat("a", 32)
	domain, err := processDomain()
	if err != nil {
		panic(err)
	}
	if err := exclusiveJSON(keyPath, claudeKey{token, start, domain}); err != nil {
		panic(err)
	}
	registry := filepath.Join(claudeSessions(), strconv.Itoa(pid)+".json")
	if err := exclusiveJSON(registry, map[string]any{"pid": pid, "messagingSocketPath": path}); err != nil {
		panic(err)
	}
	defer os.Remove(registry)
	defer os.Remove(keyPath)
	defer ln.Close()
	var mu sync.Mutex
	var messages []localFrame
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				scan := bufio.NewScanner(c)
				scan.Buffer(make([]byte, 4096), 2*pairMessageLimit+4096)
				authenticated := false
				for scan.Scan() {
					var f localFrame
					if err := json.Unmarshal(scan.Bytes(), &f); err != nil {
						panic(err)
					}
					if f.Type == "auth" {
						authenticated = f.Token == token
						continue
					}
					if !authenticated {
						panic("missing auth")
					}
					if f.Type == "user" {
						mu.Lock()
						messages = append(messages, f)
						mu.Unlock()
					}
				}
			}()
		}
	}()
	ctl, err := net.Listen("unix", filepath.Join(dir, strconv.Itoa(pid)+".ctl"))
	if err != nil {
		panic(err)
	}
	defer ctl.Close()
	info := fakeClaudeInfo{a, ctl.Addr().String()}
	if err := exclusiveJSON(filepath.Join(root, label+".json"), info); err != nil {
		panic(err)
	}
	for {
		c, err := ctl.Accept()
		if err != nil {
			panic(err)
		}
		var command fakeClaudeCommand
		if err := json.NewDecoder(c).Decode(&command); err != nil {
			panic(err)
		}
		result := fakeClaudeResult{}
		switch command.Action {
		case "pair", "unpair":
			args := []string{command.Action}
			if command.Address != "" {
				args = append(args, command.Address)
			}
			cmd := exec.Command(os.Getenv("QUACK_PAIR_BIN"), args...)
			cmd.Env = append(os.Environ(), "CLAUDE_CODE_MESSAGING_SOCKET="+path)
			output, err := cmd.CombinedOutput()
			result.Output = string(output)
			if err != nil {
				result.Error = err.Error()
			}
		case "send":
			target := strings.TrimPrefix(command.Address, "uds:")
			if !filepath.IsAbs(target) {
				paths, err := filepath.Glob(filepath.Join(claudeSessions(), "*.json"))
				if err != nil {
					panic(err)
				}
				var matches []string
				for _, path := range paths {
					raw, err := os.ReadFile(path)
					if err != nil {
						continue
					}
					var record pairRecord
					if json.Unmarshal(raw, &record) == nil && record.Name == target {
						matches = append(matches, record.Socket)
					}
				}
				if len(matches) != 1 {
					panic("ambiguous or missing SendMessage name: " + target)
				}
				target = matches[0]
			}
			peer, err := net.Dial("unix", target)
			if err != nil {
				result.Error = err.Error()
				break
			}
			credPID, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(target), ".sock"))
			if err != nil {
				panic(err)
			}
			raw, err := os.ReadFile(claudeKeyPath(credPID, target))
			if err != nil {
				panic(err)
			}
			var k claudeKey
			if err := json.Unmarshal(raw, &k); err != nil {
				panic(err)
			}
			enc := json.NewEncoder(peer)
			if err := enc.Encode(map[string]string{"type": "auth", "token": k.Token}); err != nil {
				panic(err)
			}
			f := localFrame{Type: "user", Version: 1, ID: command.ID}
			if f.ID == "" {
				f.ID = randomID()
			}
			f.Message.Content = "<cross-session-message from-mode=\"bypass\">\n" + command.Text + "\n</cross-session-message>"
			if err := enc.Encode(f); err != nil {
				panic(err)
			}
			if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				panic(err)
			}
			if err := peer.(*net.UnixConn).CloseWrite(); err != nil {
				panic(err)
			}
			if _, err := io.Copy(io.Discard, peer); err != nil {
				panic(err)
			}
			peer.Close()
		case "messages":
			mu.Lock()
			result.Messages = append([]localFrame{}, messages...)
			mu.Unlock()
		case "exit":
			json.NewEncoder(c).Encode(result)
			c.Close()
			return
		default:
			panic("unknown fake command")
		}
		if err := json.NewEncoder(c).Encode(result); err != nil {
			panic(err)
		}
		c.Close()
	}
}

func fakeCall(t *testing.T, f fakeClaudeInfo, c fakeClaudeCommand) fakeClaudeResult {
	t.Helper()
	result := fakeCallResult(t, f, c)
	if result.Error != "" {
		t.Fatalf("fake Claude %s: %s\n%s", c.Action, result.Error, result.Output)
	}
	return result
}

func fakeCallResult(t *testing.T, f fakeClaudeInfo, c fakeClaudeCommand) fakeClaudeResult {
	t.Helper()
	conn, err := net.DialTimeout("unix", f.Control, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(c); err != nil {
		t.Fatal(err)
	}
	var result fakeClaudeResult
	if err := json.NewDecoder(conn).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func fakeInfo(t *testing.T, root, label string) fakeClaudeInfo {
	t.Helper()
	var f fakeClaudeInfo
	eventually(t, "fake Claude "+label, func() bool {
		raw, err := os.ReadFile(filepath.Join(root, label+".json"))
		return err == nil && json.Unmarshal(raw, &f) == nil
	})
	return f
}

func fakeMessages(t *testing.T, f fakeClaudeInfo) []localFrame {
	return fakeCall(t, f, fakeClaudeCommand{Action: "messages"}).Messages
}

func hasFakeMessage(t *testing.T, f fakeClaudeInfo, text string) bool {
	for _, m := range fakeMessages(t, f) {
		if strings.Contains(m.Message.Content, text) {
			return true
		}
	}
	return false
}

var inboxRx = regexp.MustCompile(`Use SendMessage to "([^"]+)"`)

func fakeInbox(t *testing.T, f fakeClaudeInfo, output string) string {
	t.Helper()
	var inbox string
	eventually(t, "pair prompt", func() bool {
		text := output
		for _, m := range fakeMessages(t, f) {
			text += "\n" + m.Message.Content
		}
		matches := inboxRx.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 {
			return false
		}
		name := matches[len(matches)-1][1]
		paths, err := filepath.Glob(filepath.Join(claudeSessions(), "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var record pairRecord
			if json.Unmarshal(raw, &record) == nil && record.Name == name && record.Claude.PID == f.Claude.PID {
				inbox = "uds:" + record.Socket
				return true
			}
		}
		return false
	})
	return inbox
}

func fakeSetup(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp(os.Getenv("TMUX_TMPDIR"), "pair-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(os.Getenv("HOME"), ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(os.Getenv("HOME"), ".codex"))
	t.Setenv("QUACK_PAIR_ROOT", root)
	t.Setenv("QUACK_PAIR_BIN", bin)
	return root
}

func startFake(t *testing.T, root, label string) fakeClaudeInfo {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL="+label)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return fakeInfo(t, root, label)
}

func TestPairInboxIdentityAndCleanup(t *testing.T) {
	root := fakeSetup(t)
	fake := startFake(t, root, "identity")
	b, err := newPairInbox(fake.Claude, "Ada Lovelace", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	files := append([]string{}, b.files...)
	defer func() {
		b.close()
		for _, path := range files {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Errorf("left behind %s", path)
			}
		}
	}()
	c, err := net.Dial("unix", b.record.Socket)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(c)
	if err := enc.Encode(map[string]string{"type": "auth", "token": b.key}); err != nil {
		t.Fatal(err)
	}
	f := localFrame{Type: "user", ID: "wrong-pid"}
	f.Message.Content = "not from Claude"
	if err := enc.Encode(f); err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case <-b.messages:
		t.Fatal("accepted another process as paired Claude")
	case <-time.After(50 * time.Millisecond):
	}
	fakeCall(t, fake, fakeClaudeCommand{Action: "send", Address: b.record.Socket, Text: "intended Claude", ID: "expected"})
	select {
	case got := <-b.messages:
		if got.ID != "expected" {
			t.Fatalf("got %q", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive paired Claude")
	}
	stale := fake.Claude
	stale.Start = "stale"
	if c, err := stale.connect(); err == nil {
		c.Close()
		t.Fatal("accepted stale PID identity")
	}
}

func TestPairBridgeLimit(t *testing.T) {
	root := fakeSetup(t)
	fake := startFake(t, root, "limit")
	b, err := newPairInbox(fake.Claude, "Ada Lovelace", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	a, z := net.Pipe()
	defer a.Close()
	defer z.Close()
	p := newPairWire(a, a)
	defer close(p.done)
	remote := newPairWire(z, z)
	defer close(remote.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- bridgePair(ctx, b, p, "Ada Lovelace", nil) }()
	for n := range 30 {
		f := localFrame{ID: fmt.Sprint(n)}
		f.Message.Content = "outbound"
		b.messages <- f
		select {
		case got := <-remote.in:
			if got.Type != "message" {
				t.Fatal(got)
			}
		case <-time.After(time.Second):
			t.Fatal("bridge stalled")
		}
	}
	f := localFrame{ID: "31"}
	f.Message.Content = "over limit"
	b.messages <- f
	eventually(t, "sender limit notice", func() bool { return hasFakeMessage(t, fake, "limit of 30") })
	select {
	case got := <-remote.in:
		t.Fatalf("sent beyond limit: %+v", got)
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bridge did not stop")
	}
}

func TestNetPair(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 for tailcat pairing")
	}
	root := fakeSetup(t)
	s := server{quack(t, "new", "-n", "t-pair", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
	defer s.run("kill-server")
	defer func() {
		if t.Failed() && s.alive() {
			t.Logf("pair options at failure: %+v", s.opts(""))
		}
	}()
	host := fakeInfo(t, root, "host")
	guest := startFake(t, root, "guest")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("guest messages: %+v", fakeMessages(t, guest))
			if s.alive() {
				t.Logf("pairs: %+v waiting: %+v", pairs(s), waiting(s))
			}
		}
	})
	g := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "pair-host-term")}
	defer exec.Command(tmuxBin(), "-S", g.sock, "kill-server").Run()
	g.tmux("new-session", "-d", "-s", "host", "-x", "120", "-y", "30", bin+" attach "+s.name)
	eventually(t, "host attached", func() bool { return hostAttached(s) })
	quack(t, "share", "--pair", s.name)
	addr := inviteLinkForTest(t, s, "pair")
	started := time.Now()
	result := fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: addr})
	if time.Since(started) > 5*time.Second {
		t.Fatal("pair blocked for approval")
	}
	if !strings.Contains(result.Output, "code:") {
		t.Fatalf("missing code: %s", result.Output)
	}
	eventually(t, "pair waiting", func() bool { return len(waiting(s)) == 1 })
	if len(fakeMessages(t, host)) != 0 {
		t.Fatal("message before approval")
	}
	w := waiting(s)[0]
	if !w.pair || !strings.Contains(result.Output, w.code) {
		t.Fatalf("wrong waiter: %+v %s", w, result.Output)
	}
	status := s.must("show-options", "-gv", "status-left")
	if !strings.Contains(status, "wants to pair") {
		t.Fatal(status)
	}
	quack(t, "decline", w.code)
	eventually(t, "decline delivered", func() bool { return hasFakeMessage(t, guest, "The host declined.") && len(pairs(s)) == 0 })
	result = fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: addr})
	eventually(t, "new pair waiting", func() bool { return len(waiting(s)) == 1 })
	quack(t, "allow", waiting(s)[0].code)
	hostInbox := fakeInbox(t, host, "")
	guestInbox := fakeInbox(t, guest, result.Output)
	eventually(t, "pair shown in ls", func() bool { return strings.Contains(quack(t, "ls"), "1 pair") })
	fakeCall(t, guest, fakeClaudeCommand{Action: "send", Address: guestInbox, Text: "guest-to-host", ID: "roundtrip-one"})
	eventually(t, "guest message", func() bool { return hasFakeMessage(t, host, "guest-to-host") })
	fakeCall(t, host, fakeClaudeCommand{Action: "send", Address: hostInbox, Text: "host-to-guest"})
	eventually(t, "host message", func() bool { return hasFakeMessage(t, guest, "host-to-guest") })
	for _, f := range fakeMessages(t, host) {
		if strings.Contains(f.Message.Content, "guest-to-host") && (!strings.Contains(f.Message.Content, `from-mode="bypass"`) || strings.Count(f.Message.Content, "<cross-session-message") != 1) {
			t.Fatalf("bad local wrapper: %s", f.Message.Content)
		}
	}
	fakeCall(t, guest, fakeClaudeCommand{Action: "unpair"})
	eventually(t, "unpair cleanup", func() bool { return len(pairs(s)) == 0 && hasFakeMessage(t, host, "ended the pairing") })
	for _, path := range []string{strings.TrimPrefix(hostInbox, "uds:"), strings.TrimPrefix(guestInbox, "uds:")} {
		eventually(t, "socket removed", func() bool { _, err := os.Lstat(path); return os.IsNotExist(err) })
	}
	quack(t, "share", "--pair", "--auto-approve", "--limit", "1", s.name)
	addr = inviteLinkForTest(t, s, "pair")
	result = fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: addr})
	eventually(t, "auto pair", func() bool { return len(pairs(s)) == 1 && pairs(s)[0].state == "active" })
	_, consumedID := splitInviteLink(addr)
	consumed, _ := loadInvite(s, consumedID)
	if consumed.State != "consumed" {
		t.Fatal("pair did not consume one-time spot")
	}
	quack(t, "share", "--pair", s.name)
	addr = inviteLinkForTest(t, s, "pair")
	fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: addr})
	eventually(t, "second pair waits", func() bool { return len(waiting(s)) == 1 })

	oldestInbox := fakeInbox(t, guest, result.Output)
	n := 2
	changeInvite(s, waiting(s)[0].invite, &n, nil)
	other := startFake(t, root, "other")
	otherResult := fakeCall(t, other, fakeClaudeCommand{Action: "pair", Address: addr})
	otherInbox := fakeInbox(t, other, otherResult.Output)
	firstInbox := fakeInbox(t, guest, result.Output)
	readInbox := func(address string) pairRecord {
		path := strings.TrimPrefix(address, "uds:")
		raw, err := os.ReadFile(filepath.Join(claudeSessions(), strings.TrimSuffix(filepath.Base(path), ".sock")+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var record pairRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	firstRecord, otherRecord := readInbox(firstInbox), readInbox(otherInbox)
	if firstRecord.Identity != otherRecord.Identity {
		t.Fatalf("host changed identity between guests: %+v %+v", firstRecord.Identity, otherRecord.Identity)
	}
	if firstRecord.Name == otherRecord.Name {
		t.Fatal("local inbox aliases collide")
	}
	fakeCall(t, other, fakeClaudeCommand{Action: "send", Address: otherRecord.Name, Text: "message-from-other-session"})
	eventually(t, "named message from second guest", func() bool { return hasFakeMessage(t, host, "message-from-other-session") })
	oldestRecord := readInbox(oldestInbox)
	fakeCall(t, guest, fakeClaudeCommand{Action: "unpair", Address: oldestRecord.Name})
	eventually(t, "only the selected pairing ends", func() bool { return len(pairs(s)) == 2 })
	fakeCall(t, other, fakeClaudeCommand{Action: "send", Address: otherRecord.Name, Text: "other-session-still-paired"})
	eventually(t, "other Claude remains paired", func() bool { return hasFakeMessage(t, host, "other-session-still-paired") })

	quack(t, "unshare", s.name)
	eventually(t, "unshare cleanup", func() bool { return len(pairs(s)) == 0 && hasFakeMessage(t, guest, "The host stopped sharing.") })
	eventually(t, "all pair records removed", func() bool {
		paths, err := filepath.Glob(filepath.Join(claudeSessions(), "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var r pairRecord
			if json.Unmarshal(raw, &r) == nil && r.Entrypoint == "quack-pair" {
				return false
			}
		}
		claims, err := filepath.Glob(filepath.Join(claudeSessions(), ".quack-*.claim"))
		if err != nil {
			t.Fatal(err)
		}
		return len(claims) == 0
	})
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "quack", "keys")); !os.IsNotExist(err) {
		t.Fatal("pair saved a tunnel key")
	}
}

func TestNetPairEndings(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 for tailcat pairing")
	}
	for _, ending := range []string{"detach", "expiry", "host-exit", "guest-exit"} {
		t.Run(ending, func(t *testing.T) {
			root := fakeSetup(t)
			s := server{quack(t, "new", "-n", "t-pair-ending", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
			defer s.run("kill-server")
			host := fakeInfo(t, root, "host")
			guest := startFake(t, root, "guest")
			g := guestTerm{t, filepath.Join(os.Getenv("TMUX_TMPDIR"), "pair-ending-term")}
			defer exec.Command(tmuxBin(), "-S", g.sock, "kill-server").Run()
			g.tmux("new-session", "-d", "-s", "host", "-x", "120", "-y", "30", bin+" attach "+s.name)
			eventually(t, "host attached", func() bool { return hostAttached(s) })
			quack(t, "share", "--pair", "--auto-approve", "--expires", "8s", s.name)
			result := fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: inviteLinkForTest(t, s, "pair")})
			guestInbox := fakeInbox(t, guest, result.Output)
			hostInbox := fakeInbox(t, host, "")
			survivor := guest
			want := ""
			switch ending {
			case "detach":
				quack(t, "close", s.name)
				quack(t, "detach", s.name)
				want = "The host left, so sharing stopped."
			case "expiry":
				want = "invite expired"
			case "host-exit":
				fakeCall(t, host, fakeClaudeCommand{Action: "exit"})
				want = "ended"
			case "guest-exit":
				fakeCall(t, guest, fakeClaudeCommand{Action: "exit"})
				survivor = host
				want = "Claude exited"
			}
			eventually(t, "ending notice", func() bool { return hasFakeMessage(t, survivor, want) })
			for _, address := range []string{guestInbox, hostInbox} {
				path := strings.TrimPrefix(address, "uds:")
				eventually(t, "inbox cleanup after "+ending, func() bool { _, err := os.Lstat(path); return os.IsNotExist(err) })
			}
		})
	}
}

func TestPairDeclineGate(t *testing.T) {
	root := fakeSetup(t)
	s := server{quack(t, "new", "-n", "t-pair-decline", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
	defer s.run("kill-server")
	fakeInfo(t, root, "host")
	i := createInvite(s, "pair", -1, 0)
	c := exec.Command(bin, "_gate", s.name)
	c.Env = append(os.Environ(), "TAILCAT_PEER_KEY=nodekey:"+strings.Repeat("ab", 32), "SSH_ORIGINAL_COMMAND=pair-invite "+i.ID+" Ada Lovelace", "TAILCAT_REMOTE_ADDR=[::1]:1234")
	var stderr bytes.Buffer
	c.Stderr = &stderr
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
	defer c.Process.Kill()
	p := newPairWire(out, in)
	defer close(p.done)
	if err := p.send(pairFrame{Type: "hello", Version: pairProtocol, Identity: &agentIdentity{strings.Repeat("a", 64), "brave-otter-123456", "Ada Lovelace"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.in:
	case <-time.After(5 * time.Second):
		t.Fatal("gate did not wait")
	}
	quack(t, "decline", waiting(s)[0].code)
	select {
	case f := <-p.in:
		if f.Type != "bye" {
			t.Fatal(f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gate did not decline")
	}
	if err := c.Wait(); err != nil {
		t.Fatalf("gate cleanup: %v: %s", err, stderr.String())
	}
	if ps := pairs(s); len(ps) != 0 {
		t.Fatalf("stale pairs: %+v; options %+v", ps, s.opts(""))
	}
}
