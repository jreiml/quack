package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type invite struct {
	ID        string
	Kind      string
	Admission string
	Remaining int
	Expires   int64
	State     string
	Away      bool
	Created   int64
}

func validInviteID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == strings.ToLower(id)
}

func splitInviteLink(link string) (string, string) {
	addr, id, ok := strings.Cut(link, "/")
	if !ok || !validInviteID(id) {
		fatalf("this link needs an invite ID; ask the host for a new invite")
	}
	return addr, id
}

func (i invite) save(s host) {
	b, err := json.Marshal(i)
	if err != nil {
		fatalf("encoding invite: %v", err)
	}
	s.set("invite_"+i.ID, string(b))
}

func loadInvite(s host, id string) (invite, bool) {
	if !validInviteID(id) {
		return invite{}, false
	}
	var i invite
	raw := s.get("invite_" + id)
	if raw == "" {
		return i, false
	}
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		fatalf("reading invite: %v", err)
	}
	return i, i.ID == id
}

func invites(s host) []invite {
	var out []invite
	for id, raw := range s.opts("invite_") {
		var i invite
		if err := json.Unmarshal([]byte(raw), &i); err != nil {
			fatalf("reading invite: %v", err)
		}
		if validInviteID(id) && i.ID == id {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Created == out[b].Created {
			return out[a].ID < out[b].ID
		}
		return out[a].Created < out[b].Created
	})
	return out
}

func createInvite(s host, kind string, remaining int, expiry time.Duration) invite {
	if kind != "join" && kind != "pair" || remaining < -1 || expiry < 0 {
		fatalf("invalid invite settings")
	}
	if _, ok := s.(agentHost); ok && kind == "join" {
		fatalf("%s is an agent, not a terminal session; it can only have agent invites", s.hostName())
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		fatalf("invite ID: %v", err)
	}
	i := invite{ID: hex.EncodeToString(b), Kind: kind, State: "open", Created: time.Now().UnixNano()}
	i.setAdmission(remaining)
	if expiry > 0 {
		i.Expires = time.Now().Add(expiry).Unix()
	}
	unlock := lockShare(s)
	defer unlock()
	if s.get("closing") != "" {
		fatalf("sharing is stopping; try again")
	}
	i.save(s)
	s.set("invites_ready", "1")
	return i
}

func (i *invite) setAdmission(n int) {
	i.Admission, i.Remaining, i.Away = "ask", 0, false
	if n == 0 {
		i.Admission, i.Away = "any", true
	}
	if n > 0 {
		i.Admission, i.Remaining, i.Away = "limited", n, true
	}
}

func openInviteCount(s host) int {
	count := 0
	for _, i := range invites(s) {
		if i.State == "open" && !i.expired() {
			count++
		}
	}
	return count
}

func (i invite) command(s host) string {
	prefix := "quack join "
	if i.Kind == "pair" {
		prefix = "! quack pair "
	}
	return prefix + s.get("addr") + "/" + i.ID
}

func (i invite) expired() bool { return i.Expires != 0 && time.Now().Unix() >= i.Expires }

func (i invite) label() string {
	kind := "Terminal"
	if i.Kind == "pair" {
		kind = "Agent"
	}
	policy := "ask first"
	switch {
	case i.expired():
		policy = "expired"
	case i.State != "open":
		policy = i.State
	case i.Admission == "any":
		policy = "anyone"
	case i.Admission == "limited":
		policy = fmt.Sprintf("%d admissions left", i.Remaining)
	}
	return kind + " · " + policy
}

func (i *invite) take(s host) bool {
	if i.State != "open" || i.expired() || i.Admission == "ask" {
		return false
	}
	if i.Admission == "limited" {
		if i.Remaining <= 0 {
			return false
		}
		i.Remaining--
		if i.Remaining == 0 {
			i.State = "consumed"
		}
		i.save(s)
	}
	return true
}

func requestAdmission(s host, inviteID, kind, id, who, code string) error {
	unlock := lockShare(s)
	defer unlock()
	if s.get("closing") != "" {
		return fmt.Errorf("the host stopped sharing")
	}
	i, ok := loadInvite(s, inviteID)
	if !ok {
		return fmt.Errorf("this invite has been revoked or expired, or is invalid; ask the host for a new invite")
	}
	if i.Kind != kind {
		return fmt.Errorf("this invite does not allow this connection; ask the host for a new invite")
	}
	if i.expired() || i.State == "revoked" || i.State == "expired" {
		return fmt.Errorf("this invite has been revoked or expired")
	}
	known := s.get("member_"+id) == inviteID && s.get("ok_"+id) != ""
	if i.State == "consumed" && !known {
		return fmt.Errorf("this invite has already been used")
	}
	s.set("member_"+id, inviteID)
	s.unset("bye_" + id)
	if known || i.take(s) {
		s.set("ok_"+id, who)
	} else {
		suffix := ""
		if kind == "pair" {
			suffix = "|pair"
		}
		s.set("wait_"+id, code+"|"+who+suffix)
	}
	return nil
}

func connections(s host) []entry { return append(guests(s), pairs(s)...) }

func connected(es []entry, inviteID string) int {
	n := 0
	for _, e := range es {
		if e.invite == inviteID && e.state != "waiting" {
			n++
		}
	}
	return n
}

func disconnectEntry(e entry, reason string) {
	e.s.set("bye_"+e.hex, reason)
	e.s.unset("ok_" + e.hex)
	e.s.unset("wait_" + e.hex)
	e.s.wake(e.hex)
	if e.pair && e.pid > 0 && processRunning(e.pid, e.start) {
		if err := syscall.Kill(e.pid, syscall.SIGHUP); err != nil && err != syscall.ESRCH {
			fatalf("disconnecting %s: %v", e.name, err)
		}
	} else if s, ok := e.s.(server); ok && e.tty != "" {
		if _, err := s.run("detach-client", "-t", e.tty); err != nil {
			if strings.Contains(s.must("list-clients", "-F", "#{client_tty}"), e.tty) {
				fatalf("disconnecting %s: %v", e.name, err)
			}
		}
	}
}

func revokeInvite(s host, id string, disconnect bool, reason string) {
	unlock := lockShare(s)
	defer unlock()
	revokeInviteLocked(s, id, disconnect, reason)
}

func revokeInviteLocked(s host, id string, disconnect bool, reason string) {
	i, ok := loadInvite(s, id)
	if !ok {
		return
	}
	i.State = "revoked"
	if i.expired() {
		i.State = "expired"
	}
	i.save(s)
	active := map[string]entry{}
	for _, e := range connections(s) {
		if e.invite == id {
			active[e.hex] = e
		}
	}
	for member, owner := range s.opts("member_") {
		if owner != id {
			continue
		}
		if e, ok := active[member]; ok && e.state != "waiting" {
			if disconnect {
				disconnectEntry(e, reason)
			}
			continue
		}
		disconnectEntry(entry{s: s, hex: member}, reason)
	}
}

func stopAccess(s host, kind, reason string) {
	for _, i := range invites(s) {
		if kind == "all" || i.Kind == kind {
			revokeInvite(s, i.ID, true, reason)
		}
	}
	s.status()
}

func changeInvite(s host, id string, remaining *int, expiry *time.Duration) {
	unlock := lockShare(s)
	i, ok := loadInvite(s, id)
	if !ok || i.State == "revoked" || i.State == "expired" || i.expired() || s.get("closing") != "" {
		unlock()
		fatalf("invite is revoked or expired; create a new one")
	}
	if remaining != nil {
		i.setAdmission(*remaining)
		i.State = "open"
	}
	if expiry != nil {
		i.Expires = 0
		if *expiry > 0 {
			i.Expires = time.Now().Add(*expiry).Unix()
		}
	}
	i.save(s)
	for _, e := range waiting(s) {
		if e.invite != id {
			continue
		}
		if i.take(s) {
			admitLocked(e)
		} else if i.State == "consumed" {
			disconnectEntry(e, "The invite's admission allowance was used up.")
		}
	}
	unlock()
	s.status()
}

func staysAway(s host) bool {
	for _, i := range invites(s) {
		if !i.Away || i.expired() {
			continue
		}
		if i.State == "open" || i.State == "consumed" {
			return true
		}
		for _, e := range connections(s) {
			if e.invite == i.ID && e.state != "waiting" {
				return true
			}
		}
	}
	return false
}

func pruneInvitesLocked(s host) bool {
	changed := false
	busy := map[string]bool{}
	for id, identity := range s.opts("gate_") {
		rawPID, start, _ := strings.Cut(identity, "|")
		pid, err := strconv.Atoi(rawPID)
		alive := false
		if err == nil && pid > 0 && syscall.Kill(pid, 0) == nil {
			actual, err := processStart(pid)
			alive = err == nil && actual == start
		}
		if alive {
			busy[id] = true
			continue
		}
		s.unset("gate_" + id)
		s.wake(id)
		for _, prefix := range []string{"wait_", "bye_"} {
			s.unset(prefix + id)
		}
		for key, value := range s.opts("guest_") {
			if strings.HasPrefix(value, id+"|") {
				disconnectEntry(entry{s: s, hex: id, tty: guestTTY(key)}, "The guest connection ended.")
				s.unset("guest_" + key)
			}
		}
		for key, value := range s.opts("pid_") {
			if value == rawPID {
				s.unset("pid_" + key)
			}
		}
		changed = true
	}
	reaped := false
	for id := range s.opts("pair_") {
		if _, ok := pairWorker(s, id); !ok {
			for _, prefix := range []string{"pair_", "wait_", "ok_", "bye_", "member_"} {
				s.unset(prefix + id)
			}
			changed, reaped = true, true
		}
	}
	if reaped {
		sweepPairSockets()
	}
	for _, e := range connections(s) {
		busy[e.hex] = true
	}
	members, approved := s.opts("member_"), s.opts("ok_")
	for _, i := range invites(s) {
		if i.State == "open" {
			continue
		}
		keep := false
		for id, owner := range members {
			if owner == i.ID && (busy[id] || i.State == "consumed" && i.Kind == "join" && approved[id] != "") {
				keep = true
				break
			}
		}
		if keep {
			continue
		}
		s.unset("invite_" + i.ID)
		changed = true
		for id, owner := range members {
			if owner != i.ID {
				continue
			}
			for _, prefix := range []string{"member_", "ok_", "wait_", "bye_"} {
				s.unset(prefix + id)
			}
		}
	}
	return changed
}

func expireInvites(s host) {
	unlock := lockShare(s)
	changed := false
	for _, i := range invites(s) {
		if i.expired() && i.State != "expired" {
			revokeInviteLocked(s, i.ID, true, "The invite expired.")
			changed = true
		}
	}
	changed = pruneInvitesLocked(s) || changed
	unlock()
	if changed {
		s.status()
	}
}

func inviteExpiry(text string) time.Duration {
	if text == "never" || text == "0" {
		return 0
	}
	d, err := time.ParseDuration(text)
	if err != nil || d < time.Second {
		fatalf("expiry needs a duration like 30m or 2h, or never")
	}
	return d
}

func inviteLimit(text string) int {
	switch text {
	case "ask":
		return -1
	case "any":
		return 0
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 {
		fatalf("admission needs ask, any, or a positive number")
	}
	return n
}
