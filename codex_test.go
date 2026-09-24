package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			quack(t, "share", "--pair", "--auto-approve", s.name)
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
	peer := agentIdentity{strings.Repeat("a", 64), "brave-otter-482731", "Ada Lovelace"}
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
