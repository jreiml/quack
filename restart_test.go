package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func probeAgentInviteHost() {
	h := agentInviteHost(nil)
	if _, ok := h.(agentHost); !ok {
		panic("Codex invitation must use a detached host")
	}
	fmt.Println(h.hostName())
}

func TestCodexRestartWithMultipleThreads(t *testing.T) {
	root := fakeSetup(t)
	isolateTemp(t)
	thread, childThread := randomID(), randomID()
	fakeBin := fakeCodexBinary(t, root)
	name := quack(t, "new", "-n", "t-multiple-threads", "--", "env", "QUACK_CODEX_HELPER=1", "QUACK_PAIR_LABEL=host", "QUACK_PAIR_THREAD="+thread, "QUACK_PAIR_EXTRA_THREAD="+childThread, fakeBin)
	s := server{name}
	defer s.run("kill-server")
	fake := fakeInfo(t, root, "host")
	outside := exec.Command(fakeBin)
	outsideThread := randomID()
	outside.Env = append(os.Environ(), "QUACK_CODEX_HELPER=1", "QUACK_PAIR_LABEL=outside", "QUACK_PAIR_THREAD="+outsideThread)
	if err := outside.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { outside.Process.Kill(); outside.Wait() })
	var outsideEndpoint *codexEndpoint
	eventually(t, "unrelated Codex thread", func() bool {
		var err error
		outsideEndpoint, err = codexAt(outside.Process.Pid, outsideThread)
		return err == nil
	})
	if _, _, err := s.agent(); err == nil {
		t.Fatal("unscoped discovery unexpectedly selected one of multiple threads")
	}
	result := fakeCall(t, fake, fakeClaudeCommand{Action: "invite-host"})
	if result.Error != "" {
		t.Fatalf("exact caller invite: %s %s", result.Output, result.Error)
	}
	h := agentHost{strings.TrimSpace(result.Output)}
	_, endpoint, err := h.agent()
	if err != nil || endpoint == nil || endpoint.Thread != thread || endpoint.Exclusive {
		t.Fatalf("invite did not bind exact caller: %+v %v", endpoint, err)
	}
	agents, err := s.restartAgents()
	if err != nil || len(agents) != 2 {
		t.Fatalf("restart endpoints: %+v %v", agents, err)
	}
	quack(t, "stop", "--restart", name)
	if err := outsideEndpoint.alive(); err != nil {
		t.Fatalf("stopped unrelated Codex: %v", err)
	}
	if _, err := os.Stat(outsideEndpoint.restartPath()); !os.IsNotExist(err) {
		t.Fatal("marked unrelated thread for restart")
	}
	for _, agent := range agents {
		raw, err := os.ReadFile(agent.restartPath())
		if err != nil {
			t.Fatal(err)
		}
		var state codexRestart
		if err := json.Unmarshal(raw, &state); err != nil || state.Endpoint.Thread != agent.Thread || !state.Until.After(time.Now()) {
			t.Fatalf("thread handoff not saved: %+v %v", state, err)
		}
	}
}

func TestCodexRestartHandoff(t *testing.T) {
	root := fakeSetup(t)
	isolateTemp(t)
	thread := randomID()
	fakeBin := fakeCodexBinary(t, root)
	start := func() (*exec.Cmd, *codexEndpoint) {
		t.Helper()
		cmd := exec.Command(fakeBin)
		cmd.Env = append(os.Environ(), "QUACK_CODEX_HELPER=1", "QUACK_PAIR_LABEL=restart", "QUACK_PAIR_THREAD="+thread)
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		var endpoint *codexEndpoint
		eventually(t, "fake thread owner", func() bool {
			var err error
			endpoint, err = codexAt(cmd.Process.Pid, thread)
			return err == nil
		})
		return cmd, endpoint
	}
	old, endpoint := start()
	endpoint.Exclusive = true
	host := newAgentHost(claudeEndpoint{}, endpoint)
	record := pairRecord{Codex: endpoint}
	if err := endpoint.saveRestart(codexRestart{*endpoint, time.Now().Add(90 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	old.Process.Kill()
	old.Wait()
	if _, err := endpoint.resumed(); !errors.Is(err, errAgentRestarting) {
		t.Fatalf("handoff gap: %v", err)
	}
	if !host.alive() || record.alive() != nil {
		t.Fatal("marked restart ended pairing")
	}
	link := pairLink{b: &pairInbox{record: record, logger: log.New(io.Discard, "", 0)}}
	if reason := link.receive(pairFrame{Type: "message", Seq: 1, Text: "during restart"}); reason != "" || len(link.pending) != 1 || link.delivered != 0 {
		t.Fatalf("restart message was not retained: %s %+v", reason, link)
	}
	if reason := link.receive(pairFrame{Type: "message", Seq: 1, Text: "during restart"}); reason != "" || len(link.pending) != 1 {
		t.Fatal("retransmission duplicated a pending message")
	}
	for _, suffix := range []string{"restart.json", "restart-codex.ctl"} {
		if err := os.Remove(filepath.Join(root, suffix)); err != nil {
			t.Fatal(err)
		}
	}
	nextProcess, resumed := start()
	current, err := endpoint.resumed()
	if err != nil || current.PID != resumed.PID || current.Start != resumed.Start || !current.Exclusive {
		t.Fatalf("resumed endpoint: %+v %v", current, err)
	}
	if !record.belongsTo(claudeEndpoint{}, resumed) {
		t.Fatal("resumed caller lost pairing")
	}
	other := *resumed
	other.Thread = randomID()
	if record.belongsTo(claudeEndpoint{}, &other) {
		t.Fatal("pairing accepted a different conversation")
	}
	if found, ok := agentHostFor(claudeEndpoint{}, resumed); !ok || found != host {
		t.Fatal("resumed thread lost its agent host")
	}
	if reason := link.deliverPending(); reason != "" || len(link.pending) != 0 || link.delivered != 1 {
		t.Fatalf("retained message not delivered: %s", reason)
	}
	if reason := link.deliverPending(); reason != "" || link.delivered != 1 {
		t.Fatal("retained message delivered twice")
	}
	nextProcess.Process.Kill()
	nextProcess.Wait()
	if _, err := endpoint.resumed(); !errors.Is(err, errAgentGone) {
		t.Fatalf("ordinary stop after resume did not end pairing: %v", err)
	}
	if err := endpoint.saveRestart(codexRestart{*endpoint, time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := endpoint.resumed(); !errors.Is(err, errAgentGone) {
		t.Fatalf("expired handoff resumed: %v", err)
	}
}
