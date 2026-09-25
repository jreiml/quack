package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionIdentity(t *testing.T) {
	root := fakeSetup(t)
	first := startFake(t, root, "one")
	second := startFake(t, root, "two")
	a, err := sessionIdentity(first.Claude, "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	again, err := sessionIdentity(first.Claude, "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	other, err := sessionIdentity(second.Claude, "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	if a != again || a.Name == other.Name || a.ID == other.ID || !a.valid() {
		t.Fatalf("session identities: %+v %+v %+v", a, again, other)
	}
	s := server{quack(t, "new", "-n", "brave-otter", "--", "env", "QUACK_PAIR_HELPER=1", "QUACK_PAIR_LABEL=host", os.Args[0])}
	defer s.run("kill-server")
	host := fakeInfo(t, root, "host")
	h, err := sessionIdentity(host.Claude, "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h.Name, "brave-otter-") || !h.valid() {
		t.Fatalf("quack name not reused: %+v", h)
	}
}

func TestPeerNaming(t *testing.T) {
	root := fakeSetup(t)
	fake := startFake(t, root, "owner")
	b, err := newPairInbox(fake.Claude, "Ada Lovelace", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	peer := agentIdentity{strings.Repeat("a", 64), "brave-otter-482731", "Ada Lovelace", "Codex"}
	if err := b.setPeer(peer); err != nil {
		t.Fatal(err)
	}
	if b.record.Name != peer.Name {
		t.Fatalf("name = %s", b.record.Name)
	}
	prompt := pairPrompt(b)
	if !strings.Contains(prompt, `SendMessage to "brave-otter-482731"`) || !strings.Contains(prompt, "a Codex session belonging to Ada Lovelace") || strings.Contains(prompt, "uds:") || strings.Contains(prompt, "bypass") {
		t.Fatal(prompt)
	}
	if unknown := (agentIdentity{peer.ID, peer.Name, peer.Owner, "Gemini"}); unknown.valid() {
		t.Fatal("accepted an unknown agent type")
	}
	pairNotice(b, peer.Owner, prompt)
	if err := fake.Claude.send(b.record.Socket, peer.label(), "Hello"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "same sender for notice and message", func() bool { return len(fakeMessages(t, fake)) == 2 })
	for _, f := range fakeMessages(t, fake) {
		if !strings.Contains(f.Message.Content, `from-name="brave-otter-482731 (Ada Lovelace)"`) {
			t.Fatal(f.Message.Content)
		}
	}
	second := &pairInbox{record: pairRecord{PID: 123, SessionID: randomID(), Socket: "unused"}}
	name, err := second.claimName(peer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, path := range second.files {
			os.Remove(path)
		}
	}()
	if name == b.record.Name || !strings.HasPrefix(name, peer.Name+"-") {
		t.Fatalf("ambiguous alias: %s", name)
	}
	if b.record.Identity != peer {
		t.Fatal("routing alias changed session identity")
	}
	raw, err := os.ReadFile(filepath.Join(claudeSessions(), fmt.Sprint(os.Getpid())+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var published pairRecord
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatal(err)
	}
	if published.Identity != peer || published.Name != peer.Name {
		t.Fatalf("registry identity: %+v", published)
	}
}

func TestIdentityNameValidation(t *testing.T) {
	sum := sha256.Sum256([]byte("test"))
	for _, base := range []string{"", "Brave Otter", strings.Repeat("x", 100), "---"} {
		name := identityName(sum, base)
		if !agentNamePattern.MatchString(name) {
			t.Fatalf("invalid generated name %q", name)
		}
	}
	for _, name := range []string{"brave-otter", "brave-otter-12345", "brave-otter-abcdef", `evil"name-123456`} {
		peer := agentIdentity{strings.Repeat("a", 64), name, "Ada Lovelace", "Codex"}
		if peer.valid() {
			t.Fatalf("accepted invalid identity %q", name)
		}
	}
}
