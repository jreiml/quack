package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func (r pairRecord) belongsTo(claude claudeEndpoint, codex *codexEndpoint) bool {
	if codex != nil {
		return r.Codex != nil && r.Codex.PID == codex.PID && r.Codex.Start == codex.Start && r.Codex.Thread == codex.Thread && r.Codex.Home == codex.Home
	}
	return r.Codex == nil && r.Claude == claude
}

func sendLocal(r pairRecord, text string) error {
	if len(text) == 0 || len(text) > pairMessageLimit {
		return fmt.Errorf("message must contain 1 to %d bytes", pairMessageLimit)
	}
	if r.Codex == nil {
		return fmt.Errorf("Claude uses SendMessage to %q", r.Name)
	}
	if err := ownedPath(r.keyPath(), 0); err != nil {
		return err
	}
	raw, err := os.ReadFile(r.keyPath())
	if err != nil {
		return err
	}
	var key claudeKey
	if err := json.Unmarshal(raw, &key); err != nil {
		return err
	}
	domain, err := processDomain()
	if err != nil {
		return err
	}
	if key.Start != r.Start || key.Domain != domain || key.Token == "" {
		return fmt.Errorf("pair inbox identity changed")
	}
	a := claudeEndpoint{r.PID, r.Socket, r.Start}
	c, err := a.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	enc := json.NewEncoder(c)
	if err := enc.Encode(localFrame{Type: "auth", Token: key.Token}); err != nil {
		return err
	}
	f := localFrame{Type: "user", ID: randomID(), Session: r.Codex.Thread}
	f.Message.Role, f.Message.Content = "user", text
	if err := enc.Encode(f); err != nil {
		return err
	}
	var reply struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(c).Decode(&reply); err != nil {
		return fmt.Errorf("pair inbox did not accept the message: %w", err)
	}
	if reply.Status != "queued" {
		return fmt.Errorf("pair inbox rejected the message")
	}
	return nil
}

func cmdSend(args []string) {
	if len(args) != 3 || args[1] != "--message" {
		fatalf("usage: quack send <name> --message <text>")
	}
	caller, codex, err := callerAgent()
	if err != nil {
		fatalf("%v", err)
	}
	home := codexHome()
	if codex != nil {
		home = codex.Home
	}
	records, err := readPairRecords(home)
	if err != nil {
		fatalf("%v", err)
	}
	var found []pairRecord
	for _, r := range records {
		if !r.belongsTo(caller, codex) || r.Name != args[0] || !r.Identity.valid() {
			continue
		}
		c, err := (claudeEndpoint{r.PID, r.Socket, r.Start}).connect()
		if err != nil {
			continue
		}
		c.Close()
		found = append(found, r)
	}
	if len(found) != 1 {
		fatalf("expected one active pairing named %q for this session, found %d", args[0], len(found))
	}
	if err := sendLocal(found[0], args[2]); err != nil {
		fatalf("send: %v", err)
	}
	fmt.Printf("Queued for %s.\n", args[0])
}
