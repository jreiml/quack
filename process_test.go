package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"tailscale.com/types/key"
	"testing"
	"time"
)

func TestLinuxProcessStart(t *testing.T) {
	for _, name := range []string{"claude", "Claude Code", "name with ) and (spaces)", "name\nwith) newline"} {
		stat := "11686 (" + name + ") S " + strings.Repeat("0 ", 18) + "209661122 0 0"
		got, err := linuxProcessStart(stat)
		if err != nil || got != "209661122" {
			t.Fatalf("%q: %q %v", name, got, err)
		}
	}
	for _, stat := range []string{"", "11686 (claude) S 0", "11686 (claude) S " + strings.Repeat("0 ", 18) + "bad"} {
		if _, err := linuxProcessStart(stat); err == nil {
			t.Fatalf("accepted %q", stat)
		}
	}
}

func TestClaudeKeyProcessDomain(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(claudeSessions(), 0o700); err != nil {
		t.Fatal(err)
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	domain, err := processDomain()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if _, err := strconv.ParseUint(start, 10, 64); err != nil {
			t.Fatal("Linux start must be ticks", start)
		}
		ns, err := os.Readlink("/proc/self/ns/pid")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(domain, "linux:") || !strings.HasSuffix(domain, ":"+ns) {
			t.Fatal("invalid PID domain", domain)
		}
	}
	a := claudeEndpoint{PID: os.Getpid(), Socket: filepath.Join(t.TempDir(), "claude.sock"), Start: start}
	path := claudeKeyPath(a.PID, a.Socket)
	write := func(k claudeKey) {
		t.Helper()
		raw, err := json.Marshal(k)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(claudeKey{"test-token", start, domain})
	if token, err := a.token(); err != nil || token != "test-token" {
		t.Fatalf("matching key: %q %v", token, err)
	}
	write(claudeKey{"test-token", start, "another-domain"})
	if _, err := a.token(); err == nil {
		t.Fatal("accepted key from another process domain")
	}
	write(claudeKey{"test-token", "stale", domain})
	if _, err := a.token(); err == nil {
		t.Fatal("accepted stale process start")
	}
}

func TestNativeClaudeIdentity(t *testing.T) {
	raw := os.Getenv("QUACK_TEST_CLAUDE_PID")
	if raw == "" {
		t.Skip("set QUACK_TEST_CLAUDE_PID to validate a live Claude socket without messaging")
	}
	pid, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := endpointFor(pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := endpoint.token(); err != nil {
		t.Fatal(err)
	}
	t.Logf("Claude PID %d: process identity, private socket, peer credentials and key match", pid)
}

func TestNativeClaudePair(t *testing.T) {
	link := os.Getenv("QUACK_TEST_PAIR_LINK")
	if link == "" {
		t.Skip("set QUACK_TEST_PAIR_LINK and QUACK_TEST_CLAUDE_PID for a live pairing test")
	}
	pid, err := strconv.Atoi(os.Getenv("QUACK_TEST_CLAUDE_PID"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := endpointFor(pid)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := sessionIdentity(a, displayName())
	if err != nil {
		t.Fatal(err)
	}
	b, err := newPairInbox(a, "connecting", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	addr, id := splitInviteLink(link)
	collaborate := os.Getenv("QUACK_TEST_COLLABORATE") == "1"
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ready := false
	reason := runPairClient(ctx, pairConfig{Invite: id, Identity: identity, Addr: addr, Key: key.NewNode(), Claude: a}, b, func(f pairFrame) bool {
		if f.Type == "ready" {
			ready = true
			if collaborate {
				time.AfterFunc(25*time.Second, cancel)
				return false
			}
			for n := 1; n <= 2; n++ {
				frame := localFrame{Type: "user", ID: randomID()}
				frame.Message.Content = fmt.Sprintf("Quack Linux connectivity check %d of 2 from Codex. No action or reply needed. This temporary test pairing will disconnect shortly.", n)
				b.messages <- frame
			}
			time.AfterFunc(2*time.Second, cancel)
		} else if f.Type == "error" {
			t.Log(f.Text)
		}
		return true
	})
	if collaborate && ready {
		pairNotice(b, b.record.Peer, "Pairing ended: "+reason)
	}
	if !ready || len(b.messages) != 0 || reason != "The pairing was stopped." {
		t.Fatal("pairing did not complete the message test:", reason)
	}
	t.Log("Live pairing became ready; test message queued and temporary pairing ended:", reason)
}
