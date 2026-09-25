package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func fakeCodex() {
	root, label := os.Getenv("QUACK_PAIR_ROOT"), os.Getenv("QUACK_PAIR_LABEL")
	if len(os.Args) > 1 && os.Args[1] == "queue" {
		root = filepath.Dir(os.Getenv("CODEX_HOME"))
		label = strings.TrimSuffix(filepath.Base(os.Getenv("CODEX_HOME")), "-codex-home")
		if len(os.Args) != 6 || os.Args[2] != "--thread" || os.Args[4] != "--message" {
			panic("bad queue invocation")
		}
		f, err := os.OpenFile(filepath.Join(root, label+"-messages"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			panic(err)
		}
		if err := json.NewEncoder(f).Encode(os.Args[5]); err != nil {
			panic(err)
		}
		f.Close()
		return
	}
	thread := randomID()
	if err := os.Setenv("CODEX_THREAD_ID", thread); err != nil {
		panic(err)
	}
	home := filepath.Join(root, label+"-codex-home")
	if err := os.Setenv("CODEX_HOME", home); err != nil {
		panic(err)
	}
	dir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	lock, err := os.Create(filepath.Join(dir, thread+".lock"))
	if err != nil {
		panic(err)
	}
	defer lock.Close()
	if os.Getenv("QUACK_CODEX_NO_ROLLOUT") != "1" {
		writeCodexRollout(home, thread)
	}
	path := filepath.Join(root, label+"-codex.ctl")
	ln, err := net.Listen("unix", path)
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	info := fakeClaudeInfo{Claude: claudeEndpoint{PID: os.Getpid()}, Control: path}
	if err := exclusiveJSON(filepath.Join(root, label+".json"), info); err != nil {
		panic(err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			panic(err)
		}
		var cmd fakeClaudeCommand
		if err := json.NewDecoder(c).Decode(&cmd); err != nil {
			panic(err)
		}
		result := fakeClaudeResult{}
		switch cmd.Action {
		case "pair", "unpair", "send":
			args := []string{cmd.Action}
			if cmd.Address != "" {
				args = append(args, cmd.Address)
			}
			if cmd.Action == "send" {
				args = append(args, "--message", cmd.Text)
			}
			out, err := exec.Command(os.Getenv("QUACK_PAIR_BIN"), args...).CombinedOutput()
			result.Output = string(out)
			if err != nil {
				result.Error = err.Error()
			}
		case "messages":
			f, err := os.Open(filepath.Join(root, label+"-messages"))
			if err != nil && !os.IsNotExist(err) {
				panic(err)
			}
			if err == nil {
				scan := bufio.NewScanner(f)
				scan.Buffer(make([]byte, 4096), 4*pairMessageLimit)
				for scan.Scan() {
					var text string
					if err := json.Unmarshal(scan.Bytes(), &text); err != nil {
						panic(err)
					}
					frame := localFrame{}
					frame.Message.Content = text
					result.Messages = append(result.Messages, frame)
				}
				if err := scan.Err(); err != nil {
					panic(err)
				}
				f.Close()
			}
		case "exit":
			if err := json.NewEncoder(c).Encode(result); err != nil {
				panic(err)
			}
			c.Close()
			return
		default:
			panic("unknown fake Codex command")
		}
		if err := json.NewEncoder(c).Encode(result); err != nil {
			panic(err)
		}
		c.Close()
	}
}

func writeCodexRollout(home, thread string) {
	dir := filepath.Join(home, "sessions", "2026", "09", "25")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-25T10-31-27-"+thread+".jsonl"), nil, 0o600); err != nil {
		panic(err)
	}
}

func fakeCodexBinary(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "codex")
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexThreads(t *testing.T) {
	id := randomID()
	got := codexThreads([]string{"/home/ada/.codex/thread-writer-locks/" + id + ".lock", "/tmp/not-a-thread.lock", "/tmp/thread-writer-locks/bad.lock"})
	if len(got) != 1 || got[id] != "/home/ada/.codex" {
		t.Fatal(got)
	}
}

func TestCodexHasRollout(t *testing.T) {
	home, thread := t.TempDir(), randomID()
	if codexHasRollout(home, thread) {
		t.Fatal("found rollout in empty home")
	}
	writeCodexRollout(home, randomID())
	if codexHasRollout(home, thread) {
		t.Fatal("matched another thread's rollout")
	}
	writeCodexRollout(home, thread)
	if !codexHasRollout(home, thread) {
		t.Fatal("missed rollout")
	}
}

func TestCodexAliveOnlyEndsForGoneAgents(t *testing.T) {
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]codexEndpoint{
		"exited":    {PID: exited.Process.Pid, Start: start, Thread: "t"},
		"reused":    {PID: os.Getpid(), Start: "not " + start, Thread: "t"},
		"no thread": {PID: os.Getpid(), Start: start, Thread: "t", Home: t.TempDir()},
	} {
		if err := a.alive(); !errors.Is(err, errAgentGone) {
			t.Errorf("%s: alive() = %v, want errAgentGone", name, err)
		}
	}
}

func TestCodexSandboxRefusal(t *testing.T) {
	link := "tcExample/" + strings.Repeat("ab", 16)
	c := exec.Command(bin, "pair", link)
	c.Env = append(os.Environ(), "CODEX_SANDBOX=seatbelt")
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Codex's sandbox blocks this") || !strings.Contains(string(out), "! quack pair "+link) {
		t.Fatalf("%v\n%s", err, out)
	}
	c = exec.Command(bin, "unpair", "brave-otter-482731")
	c.Env = append(os.Environ(), "CODEX_SANDBOX=seatbelt")
	out, err = c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "! quack unpair brave-otter-482731") {
		t.Fatalf("unpair: %v\n%s", err, out)
	}
	c = exec.Command(bin, "pair", link)
	c.Env = append(os.Environ(), "CODEX_SANDBOX_NETWORK_DISABLED=1", "CLAUDE_CONFIG_DIR="+t.TempDir(), "CLAUDE_CODE_MESSAGING_SOCKET=", "CODEX_THREAD_ID=")
	out, err = c.CombinedOutput()
	if err == nil {
		t.Fatalf("paired without an agent: %s", out)
	}
	if strings.Contains(string(out), "sandbox") {
		t.Fatalf("escalated Codex commands keep CODEX_SANDBOX_NETWORK_DISABLED: %s", out)
	}
}

func codexQueuedTexts(t *testing.T, db, thread string) []string {
	t.Helper()
	out, err := exec.Command("sqlite3", "-readonly", db, "select payload_json from queued_items where thread_id = '"+thread+"' order by queue_order").Output()
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var payload struct {
			UserInput struct {
				Content []struct{ Text string } `json:"content"`
			}
		}
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, payload.UserInput.Content[0].Text)
	}
	return texts
}

func TestCodexQueueFirst(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is needed to start a Codex thread")
	}
	a := codexEndpoint{Home: t.TempDir(), Thread: randomID()}
	if err := a.queueFirst("hello"); err == nil || !strings.Contains(err.Error(), "queue_1.sqlite") {
		t.Fatalf("created a queue database: %v", err)
	}
	db := filepath.Join(a.Home, "queue_1.sqlite")
	if out, err := exec.Command("sqlite3", db, "CREATE TABLE queued_items (id TEXT PRIMARY KEY NOT NULL, thread_id TEXT NOT NULL, payload_json TEXT NOT NULL, queue_order INTEGER NOT NULL, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL); CREATE UNIQUE INDEX queued_items_thread_order_idx ON queued_items(thread_id, queue_order);").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	texts := []string{"it's \"quoted\"\n'); DROP TABLE queued_items; --", "second"}
	for _, text := range texts {
		if err := a.queueFirst(text); err != nil {
			t.Fatal(err)
		}
	}
	if got := codexQueuedTexts(t, db, a.Thread); len(got) != 2 || got[0] != texts[0] || got[1] != texts[1] {
		t.Fatalf("queued %q", got)
	}
}

func TestNetCodexHostWithoutMessages(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 for tailcat pairing")
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is needed to start a Codex thread")
	}
	root := fakeSetup(t)
	fakeBin := fakeCodexBinary(t, root)
	home := filepath.Join(root, "host-codex-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(home, "queue_1.sqlite")
	schema := "CREATE TABLE queued_items (id TEXT PRIMARY KEY NOT NULL, thread_id TEXT NOT NULL, payload_json TEXT NOT NULL, queue_order INTEGER NOT NULL, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL); CREATE UNIQUE INDEX queued_items_thread_order_idx ON queued_items(thread_id, queue_order);"
	if out, err := exec.Command("sqlite3", db, schema).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	s := server{quack(t, "new", "-n", "t-codex-fresh", "--", "env", "QUACK_CODEX_HELPER=1", "QUACK_CODEX_NO_ROLLOUT=1", "QUACK_PAIR_LABEL=host", fakeBin)}
	host := fakeInfo(t, root, "host")
	defer s.run("kill-server")
	guest := startFake(t, root, "guest")
	quack(t, "invite", "new", "agent", "--auto-approve", "-n", s.name)
	fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: inviteLinkForTest(t, s, "pair")})
	eventually(t, "pair active", func() bool { return len(pairs(s)) == 1 && pairs(s)[0].state == "active" })
	locks, err := filepath.Glob(filepath.Join(home, "thread-writer-locks", "*.lock"))
	if err != nil || len(locks) != 1 {
		t.Fatalf("thread locks: %v %v", locks, err)
	}
	thread := strings.TrimSuffix(filepath.Base(locks[0]), ".lock")
	eventually(t, "first message queued for the unstarted thread", func() bool {
		texts := codexQueuedTexts(t, db, thread)
		return len(texts) == 1 && strings.Contains(texts[0], "quack send ")
	})
	if hasFakeMessage(t, host, "quack send ") {
		t.Fatal("used codex queue before the thread started")
	}
	var guestName string
	records, err := readPairRecords()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Claude.PID == guest.Claude.PID && r.Identity.valid() {
			guestName = r.Name
		}
	}
	if guestName == "" {
		t.Fatal("no guest inbox")
	}
	writeCodexRollout(home, thread)
	fakeCall(t, guest, fakeClaudeCommand{Action: "send", Address: guestName, Text: "after-start"})
	eventually(t, "delivery after the thread started", func() bool { return hasFakeMessage(t, host, "after-start") })
	if n := len(codexQueuedTexts(t, db, thread)); n != 1 {
		t.Fatalf("wrote %d queue rows after the thread started", n)
	}
}

func TestNativeCodexDiscovery(t *testing.T) {
	if os.Getenv("QUACK_TEST_CODEX") != "1" {
		t.Skip("set QUACK_TEST_CODEX=1 to inspect the calling Codex without messaging")
	}
	a, err := callerCodex()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.alive(); err != nil {
		t.Fatal(err)
	}
	t.Logf("Codex PID %d, thread %s, binary %s", a.PID, a.Thread, a.Binary)
}

func TestNetCodexPair(t *testing.T) {
	if os.Getenv("QUACK_NET_TEST") == "" {
		t.Skip("set QUACK_NET_TEST=1 for tailcat pairing")
	}
	for _, kinds := range []struct{ host, guest bool }{{false, true}, {true, false}, {true, true}} {
		codexHost := kinds.host
		t.Run(fmt.Sprintf("codex-host-%t-guest-%t", kinds.host, kinds.guest), func(t *testing.T) {
			root := fakeSetup(t)
			writeNativeClaudeRecord(t)
			fakeBin := fakeCodexBinary(t, root)
			var host, guest fakeClaudeInfo
			var s server
			if codexHost {
				s = server{quack(t, "new", "-n", fmt.Sprintf("t-codex-host-%t", kinds.guest), "--", "env", "QUACK_CODEX_HELPER=1", "QUACK_PAIR_LABEL=host", fakeBin)}
				host = fakeInfo(t, root, "host")
			} else {
				s = server{quack(t, "new", "-n", "t-claude-host", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
				host = fakeInfo(t, root, "host")
			}
			if !kinds.guest {
				guest = startFake(t, root, "guest")
			} else {
				c := exec.Command(fakeBin)
				c.Env = append(os.Environ(), "QUACK_CODEX_HELPER=1", "QUACK_PAIR_LABEL=guest")
				c.Stderr = os.Stderr
				if err := c.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Process.Kill(); c.Wait() })
				guest = fakeInfo(t, root, "guest")
			}
			defer s.run("kill-server")
			defer func() {
				if t.Failed() {
					t.Logf("host messages: %+v", fakeMessages(t, host))
					t.Logf("guest messages: %+v", fakeMessages(t, guest))
					t.Logf("pairs: %+v", pairs(s))
					paths, err := filepath.Glob(filepath.Join(os.TempDir(), "quack", "pair-*.log"))
					if err != nil {
						t.Log(err)
						return
					}
					for _, path := range paths {
						raw, err := os.ReadFile(path)
						if err != nil {
							t.Log(err)
							continue
						}
						t.Logf("%s: %s", path, raw)
					}
				}
			}()
			quack(t, "invite", "new", "agent", "--auto-approve", "-n", s.name)
			result := fakeCall(t, guest, fakeClaudeCommand{Action: "pair", Address: inviteLinkForTest(t, s, "pair")})
			var hostName, guestName string
			eventually(t, "pair ready", func() bool {
				if len(pairs(s)) != 1 || pairs(s)[0].state != "active" {
					return false
				}
				records, err := readPairRecords()
				if err != nil {
					t.Fatal(err)
				}
				paths, err := filepath.Glob(filepath.Join(root, "*-codex-home", "quack-pairs", "*.json"))
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var r pairRecord
					if err := json.Unmarshal(raw, &r); err != nil {
						t.Fatal(err)
					}
					records = append(records, r)
				}
				for _, r := range records {
					if !r.Identity.valid() {
						continue
					}
					pid := r.Claude.PID
					if r.Codex != nil {
						pid = r.Codex.PID
					}
					if pid == host.Claude.PID {
						hostName = r.Name
					} else if pid == guest.Claude.PID {
						guestName = r.Name
					}
				}
				return hostName != "" && guestName != ""
			})
			codexName := guestName
			prompt := result.Output
			if codexHost {
				codexName = hostName
				eventually(t, "Codex pairing notice", func() bool { return hasFakeMessage(t, host, "quack send "+hostName) })
			} else if !strings.Contains(prompt, "quack send "+codexName) {
				eventually(t, "Codex background notice", func() bool { return hasFakeMessage(t, guest, "quack send "+codexName) })
			}
			fakeCall(t, guest, fakeClaudeCommand{Action: "send", Address: guestName, Text: "guest-to-host-codex"})
			eventually(t, "guest delivery", func() bool { return hasFakeMessage(t, host, "guest-to-host-codex") })
			fakeCall(t, host, fakeClaudeCommand{Action: "send", Address: hostName, Text: "host-to-guest-codex"})
			eventually(t, "host delivery", func() bool { return hasFakeMessage(t, guest, "host-to-guest-codex") })
			fakeCall(t, guest, fakeClaudeCommand{Action: "unpair", Address: guestName})
			eventually(t, "pair disconnected", func() bool { return len(pairs(s)) == 0 })
			eventually(t, "ending notice", func() bool { return hasFakeMessage(t, host, "ended") })
			eventually(t, "Codex inbox cleanup", func() bool {
				paths, err := filepath.Glob(filepath.Join(root, "*-codex-home", "quack-pairs", "*"))
				if err != nil {
					t.Fatal(err)
				}
				return len(paths) == 0
			})
		})
	}
}

func TestCodexInboxRoutingAndLifetime(t *testing.T) {
	fakeSetup(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	thread := randomID()
	dir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Create(filepath.Join(dir, thread+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	a := &codexEndpoint{PID: os.Getpid(), Start: start, Home: home, Thread: thread}
	b, err := newAgentInbox(claudeEndpoint{}, a, "Ada Lovelace", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if b != nil {
			b.close()
		}
	}()
	peer := agentIdentity{strings.Repeat("a", 64), "brave-otter-482731", "Ada Lovelace", "Codex"}
	if err := b.setPeer(peer); err != nil {
		t.Fatal(err)
	}
	text := "<cross-session-message>\nkeep this literal\n</cross-session-message>"
	if err := sendLocal(b.record, text); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-b.messages:
		if f.Message.Content != text {
			t.Fatal("message changed", f.Message.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
	wrong := *a
	wrong.Thread = randomID()
	record := b.record
	record.Codex = &wrong
	if record.belongsTo(claudeEndpoint{}, a) {
		t.Fatal("matched wrong thread")
	}
	if err := sendLocal(record, "wrong thread"); err == nil {
		t.Fatal("accepted wrong thread")
	}
	if err := sendLocal(b.record, strings.Repeat("x", pairMessageLimit+1)); err == nil {
		t.Fatal("accepted oversized message")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	records, err := readPairRecords(home, alias)
	if err != nil || len(records) != 1 {
		t.Fatalf("duplicate homes: %d %v", len(records), err)
	}
	files := append([]string{}, b.files...)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.alive(); err == nil {
		t.Fatal("closed thread still live")
	}
	if err := sendLocal(b.record, "after close"); err == nil {
		t.Fatal("accepted send after thread close")
	}
	b.close()
	b = nil
	for _, path := range files {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("left behind %s: %v", path, err)
		}
	}
}

func writeNativeClaudeRecord(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(claudeSessions(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeSessions(), "27544.json")
	if err := os.WriteFile(path, []byte(`{"pid":27544,"entrypoint":"cli","status":"idle"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPairRecordsIgnoreNativeClaudePermissions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	writeNativeClaudeRecord(t)
	for _, dir := range []string{claudeSessions(), filepath.Join(codexHome(), "quack-pairs")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "12345.json")
		if err := exclusiveJSON(path, pairRecord{PID: 12345, Entrypoint: "quack-pair"}); err != nil {
			t.Fatal(err)
		}
		records, err := readPairRecords()
		if err != nil || len(records) != 1 || records[0].PID != 12345 {
			t.Fatalf("pair lookup: %+v, %v", records, err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readPairRecords(); err == nil || !strings.Contains(err.Error(), path+" must be private") {
			t.Fatalf("accepted public quack record: %v", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(filepath.Join(claudeSessions(), "27544.json"))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("native Claude permissions changed: %v, %v", st, err)
	}
}

func fakeAppServer(t *testing.T, path string, handle func(method string, params json.RawMessage) any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.CloseNow()
		for {
			var req struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := wsjson.Read(r.Context(), ws, &req); err != nil {
				return
			}
			result := handle(req.Method, req.Params)
			if req.ID == nil {
				continue
			}
			wsjson.Write(r.Context(), ws, map[string]any{"method": "thread/status/changed", "params": map[string]any{}})
			wsjson.Write(r.Context(), ws, map[string]any{"id": *req.ID, "method": "item/tool/requestUserInput", "params": map[string]any{}})
			if err, ok := result.(error); ok {
				wsjson.Write(r.Context(), ws, map[string]any{"id": *req.ID, "error": map[string]any{"code": -32600, "message": err.Error()}})
				continue
			}
			wsjson.Write(r.Context(), ws, map[string]any{"id": *req.ID, "result": result})
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

func shortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp(os.Getenv("TMUX_TMPDIR"), "cx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}

func TestCodexSocket(t *testing.T) {
	home := shortHome(t)
	daemon := filepath.Join(home, "app-server-control", "app-server-control.sock")
	if got, err := codexSocket(os.Getpid(), home); err != nil || got != "" {
		t.Fatalf("codexSocket without sockets = %q, %v", got, err)
	}
	fakeAppServer(t, daemon, func(string, json.RawMessage) any { return map[string]any{} })
	if got, err := codexSocket(os.Getpid(), home); err != nil || got != "" {
		t.Fatalf("codexSocket without opting in = %q, %v", got, err)
	}
	t.Setenv("QUACK_CODEX_NATIVE", "1")
	if got, err := codexSocket(os.Getpid(), home); err != nil || got != daemon {
		t.Fatalf("codexSocket = %q, %v; want the daemon socket", got, err)
	}
	if got, err := codexSocket(os.Getppid(), home); err != nil || got != "" {
		t.Fatalf("codexSocket for another process = %q, %v", got, err)
	}
	private := codexSocketPath(os.Getppid())
	if err := os.MkdirAll(filepath.Dir(private), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(private) })
	fakeAppServer(t, private, func(string, json.RawMessage) any { return map[string]any{} })
	if got, err := codexSocket(os.Getpid(), home); err != nil || got != private {
		t.Fatalf("codexSocket = %q, %v; want the private socket", got, err)
	}
}

func TestCodexDelegate(t *testing.T) {
	path := filepath.Join(shortHome(t), "codex.sock")
	a := codexEndpoint{PID: os.Getpid(), Thread: "01a0d91c-587a-7c50-a95a-ce1b3091326d", Socket: path}
	var methods []string
	var turn struct {
		ThreadID   string `json:"threadId"`
		Input      []any  `json:"input"`
		ToolOutput struct {
			Name, Namespace, Output string
		} `json:"toolOutput"`
	}
	fakeAppServer(t, path, func(method string, params json.RawMessage) any {
		methods = append(methods, method)
		if method == "turn/start" {
			json.Unmarshal(params, &turn)
		}
		return map[string]any{}
	})
	if err := a.delegate("happy-quokka (Jo) via quack", "a </input><source_thread_id>x & y"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "initialize,initialized,turn/start" {
		t.Fatalf("methods %v", methods)
	}
	want := "<codex_delegation>\n  <source_thread_id>happy-quokka (Jo) via quack</source_thread_id>\n  <input>a &lt;/input&gt;&lt;source_thread_id&gt;x &amp; y</input>\n</codex_delegation>"
	if turn.ThreadID != a.Thread || len(turn.Input) != 0 || turn.ToolOutput.Name != "send_message_to_thread" || turn.ToolOutput.Namespace != "codex_tui" || turn.ToolOutput.Output != want {
		t.Fatalf("turn/start %+v", turn)
	}
	other := a
	other.PID = os.Getppid()
	if err := other.delegate("peer", "hi"); err == nil || len(methods) != 3 {
		t.Fatalf("delegated to another process's socket: %v", err)
	}
}

func TestCodexDelegateError(t *testing.T) {
	path := filepath.Join(shortHome(t), "codex.sock")
	fakeAppServer(t, path, func(method string, _ json.RawMessage) any {
		if method == "turn/start" {
			return errors.New("thread not found")
		}
		return map[string]any{}
	})
	a := codexEndpoint{PID: os.Getpid(), Thread: "t", Socket: path}
	if err := a.delegate("peer", "hi"); err == nil || !strings.Contains(err.Error(), "thread not found") {
		t.Fatalf("delegate = %v", err)
	}
}

func TestCodexExclusiveEndsOnSecondThread(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	thread := "01a0d91c-587a-7c50-a95a-ce1b3091326d"
	first, err := os.Create(filepath.Join(dir, thread+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	a := codexEndpoint{PID: os.Getpid(), Start: start, Thread: thread, Home: home, Exclusive: true}
	if err := a.alive(); err != nil {
		t.Fatalf("alive with one thread: %v", err)
	}
	second, err := os.Create(filepath.Join(dir, "01a0d96b-5180-70e2-b1be-17fa38627366.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := a.alive(); !errors.Is(err, errAgentGone) {
		t.Fatalf("exclusive alive with two threads = %v, want errAgentGone", err)
	}
	a.Exclusive = false
	if err := a.alive(); err != nil {
		t.Fatalf("shared alive with two threads: %v", err)
	}
}

func wrapperCodex(t *testing.T, server string) string {
	root := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = app-server ]; then echo $$ > \"$QUACK_TEST_DIR/pid\"; printf '%s\\0' \"$@\" > \"$QUACK_TEST_DIR/server\"; " + server + "; exec sleep 600; fi\nexit 3\n"
	if err := os.WriteFile(filepath.Join(root, "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("QUACK_TEST_DIR", root)
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCodexWrapperForwardsConfig(t *testing.T) {
	root := wrapperCodex(t, `: > "${3#unix://}"`)
	cmd := exec.Command(bin, "_codex", "--model", "gpt-6", "-c", "a=1", "--config=b=2", "-cc=3", "--enable", "x", "--disable=y", "resume", "--", "-c", "d=4")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("wrapper: %s %v", out, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "server"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")[3:]
	if want := []string{"-c", "a=1", "--config=b=2", "-cc=3", "--enable", "x", "--disable=y"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("server arguments %q, want %q", got, want)
	}
	for _, profile := range []string{"-p", "--profile", "-pwork", "--profile=work"} {
		out, err := exec.Command(bin, "_codex", profile, "work").CombinedOutput()
		if err == nil || !strings.Contains(string(out), "profile") {
			t.Fatalf("%s: %s %v", profile, out, err)
		}
	}
}

func TestCodexWrapperStopsServerOnSignalDuringStartup(t *testing.T) {
	root := wrapperCodex(t, ":")
	cmd := exec.Command(bin, "_codex")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	eventually(t, "app server", func() bool {
		raw, err := os.ReadFile(filepath.Join(root, "pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 0
	})
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Fatalf("wrapper: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("app server %d survived: %v", pid, err)
	}
}
