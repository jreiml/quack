package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

func codeFor(pub string) string {
	sum := sha256.Sum256([]byte(pub))
	return codeWords[sum[0]] + "-" + codeWords[sum[1]]
}

func normalizeCode(args []string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(strings.Join(args, " "), "-", " ")), "-"))
}

func channel(hex string) string { return "quack-" + hex[:16] }

func cleanName(raw string) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" .-_@'", r) {
			b.WriteRune(r)
		}
	}
	name := strings.Join(strings.Fields(b.String()), " ")
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}
	if name == "" {
		return "someone"
	}
	return name
}

type entry struct {
	s      server
	hex    string
	code   string
	name   string
	tty    string
	pid    int
	pair   bool
	invite string
	state  string
}

func waiting(s server) []entry {
	var out []entry
	for hex, v := range s.opts("wait_") {
		f := strings.SplitN(v, "|", 3)
		if len(f) < 2 {
			continue
		}
		out = append(out, entry{s: s, hex: hex, invite: s.get("member_" + hex), code: f[0], name: f[1], pair: len(f) == 3 && f[2] == "pair"})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out
}

func guests(s server) []entry {
	var out []entry
	for id, v := range s.opts("guest_") {
		f := strings.SplitN(v, "|", 4)
		if len(f) != 4 {
			continue
		}
		pid, err := strconv.Atoi(f[3])
		if err != nil {
			continue
		}
		out = append(out, entry{s: s, hex: f[0], invite: s.get("member_" + f[0]), name: f[1], code: f[2], tty: "/dev/" + id, pid: pid})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out
}

func guestTTYs(s server) map[string]bool {
	m := map[string]bool{}
	for _, g := range guests(s) {
		m[g.tty] = true
	}
	return m
}

func statusEscape(s string) string {
	return strings.NewReplacer("#", "##", ",", "#,", "}", "#}").Replace(s)
}

func statusLine(segments []string) string {
	return strings.Join(segments, "   ")
}

func refreshStatus(s server) {
	gs := guests(s)
	shared := s.shared()
	ws := waiting(s)
	namesExcept := func(tty string) []string {
		var names []string
		seen := map[string]bool{}
		for _, g := range gs {
			if g.tty != tty && !seen[g.name] {
				seen[g.name] = true
				names = append(names, g.name)
			}
		}
		return names
	}
	host := []string{"🦆 Ctrl-Q"}
	if shared {
		host = append(host, statusEscape("🌐 "+plural(openInviteCount(s), "invite")))
	}
	if names := namesExcept(""); len(names) > 0 {
		host = append(host, statusEscape("👀 "+strings.Join(names, ", ")))
	}
	for _, p := range pairs(s) {
		if p.state == "active" {
			host = append(host, statusEscape("🤖 "+p.name))
		}
	}
	for _, w := range ws {
		verb := "join"
		if w.pair {
			verb = "pair"
		}
		host = append(host, "#[bg=colour220#,fg=colour16] "+statusEscape(fmt.Sprintf("✋ %s wants to %s (code %s)", w.name, verb, w.code))+" #[default]")
	}
	format := statusLine(host)
	for id, note := range s.opts("note_") {
		format = fmt.Sprintf("#{?#{==:#{client_tty},/dev/%s},%s,%s}", id, statusLine(append(slices.Clone(host), statusEscape(note))), format)
	}
	for _, g := range gs {
		guest := []string{"🦆 Ctrl-Q"}
		if h := s.get("host"); h != "" {
			guest = append(guest, statusEscape("🏠 "+h))
		}
		if names := namesExcept(g.tty); len(names) > 0 {
			guest = append(guest, statusEscape("👀 "+strings.Join(names, ", ")))
		}
		format = fmt.Sprintf("#{?#{==:#{client_tty},%s},%s,%s}", g.tty, statusLine(guest), format)
	}
	s.must("set-option", "-g", "status-left", " "+format+" ")
	fitHost(s)
}

func notify(msg string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	c := exec.Command("/usr/bin/osascript", "-e", fmt.Sprintf(`display notification "%s" with title "quack"`, q.Replace(msg)))
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		return err
	}
	go c.Wait()
	return nil
}

func ttyName() string {
	c := exec.Command("/usr/bin/tty")
	c.Stdin = os.Stdin
	out, err := c.Output()
	if err != nil {
		fatalf("finding tty: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func cmdGate(args []string) {
	if len(args) != 1 {
		fatalf("usage: quack _gate <name>")
	}
	s := server{args[0]}
	logger, _ := openLog(s.name)
	pub := os.Getenv("TAILCAT_PEER_KEY")
	if !strings.HasPrefix(pub, "nodekey:") {
		logger.Fatalf("gate: missing TAILCAT_PEER_KEY")
	}
	command, rest, _ := strings.Cut(strings.TrimSpace(os.Getenv("SSH_ORIGINAL_COMMAND")), " ")
	inviteID, name, _ := strings.Cut(rest, " ")
	if !validInviteID(inviteID) {
		logger.Fatalf("missing invite ID; ask the host for a new invite")
	}
	if command == "pair-invite" {
		if syscall.Getpgrp() != os.Getpid() {
			if err := syscall.Setpgid(0, 0); err != nil {
				logger.Fatalf("pair process group: %v", err)
			}
		}
		pairGate(s, pub, cleanName(name), inviteID)
		return
	}
	if command != "join-invite" {
		logger.Fatalf("unsupported guest command")
	}
	sum := sha256.Sum256([]byte(pub + ":" + inviteID))
	hex := fmt.Sprintf("%x", sum)
	who := cleanName(name)
	code := codeFor(pub)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	s.run("set-option", "-gu", "@quack_bye_"+hex)
	pidOpt := "@quack_pid_" + connID(os.Getenv("TAILCAT_REMOTE_ADDR"))
	s.must("set-option", "-g", pidOpt, strconv.Itoa(os.Getpid()))
	start, err := processStart(os.Getpid())
	if err != nil {
		logger.Fatalf("gate identity: %v", err)
	}
	s.set("gate_"+hex, strconv.Itoa(os.Getpid())+"|"+start)
	leave := func(code int) {
		s.run("set-option", "-gu", "@quack_gate_"+hex)
		s.run("set-option", "-gu", pidOpt)
		os.Exit(code)
	}

	if err := requestAdmission(s, inviteID, "join", hex, who, code); err != nil {
		fmt.Printf("\r\n  %s\r\n", err)
		leave(1)
	}
	if s.get("ok_"+hex) == "" {
		refreshStatus(s)
		logger.Printf("%s (%s) waiting", who, code)
		if err := notify(fmt.Sprintf("%s wants to join %s (code %s). Ctrl-Q to answer.", who, s.name, code)); err != nil {
			logger.Printf("notify: %v", err)
		}
		fmt.Printf("\r\n  Waiting for the host to let you in.\r\n  Send them this code:  %s\r\n\r\n  Ctrl-C cancels.\r\n", code)
		for s.get("ok_"+hex) == "" {
			if !s.alive() {
				fmt.Printf("  The host ended the session.\r\n")
				leave(1)
			}
			if s.get("wait_"+hex) == "" {
				sayBye(s, hex, "The host declined.")
				leave(1)
			}
			wait := s.cmd("wait-for", channel(hex))
			if err := wait.Start(); err != nil {
				logger.Fatalf("wait-for: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- wait.Wait() }()
			select {
			case err := <-done:
				if err != nil && s.alive() {
					logger.Fatalf("wait-for: %v", err)
				}
			case <-hup:
				wait.Process.Kill()
				if _, err := s.run("set-option", "-gu", "@quack_wait_"+hex); err == nil {
					refreshStatus(s)
				}
				logger.Printf("%s (%s) gave up waiting", who, code)
				leave(0)
			}
		}
	}

	id := filepath.Base(ttyName())
	s.set("guest_"+id, hex+"|"+who+"|"+code+"|"+strconv.Itoa(os.Getpid()))
	refreshStatus(s)
	logger.Printf("%s (%s) joined on %s", who, code, id)
	if err := notify(fmt.Sprintf("%s joined %s", who, s.name)); err != nil {
		logger.Printf("notify: %v", err)
	}
	select {
	case <-hup:
		s.run("set-option", "-gu", "@quack_guest_"+id)
		leave(0)
	default:
	}
	if s.get("ok_"+hex) == "" {
		s.run("set-option", "-gu", "@quack_guest_"+id)
		refreshStatus(s)
		sayBye(s, hex, "The host removed you.")
		leave(1)
	}
	attach := s.cmd("attach-session", "-t", "=main")
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := attach.Run(); err != nil {
		logger.Printf("%s attach: %v", who, err)
	}
	if !s.alive() {
		fmt.Printf("\r\n  The host ended the session.\r\n")
	} else {
		s.run("set-option", "-gu", "@quack_guest_"+id)
		refreshStatus(s)
		sayBye(s, hex, "")
	}
	logger.Printf("%s (%s) left", who, code)
	leave(0)
}

func allEntries(list func(server) []entry) []entry {
	var out []entry
	for _, s := range servers() {
		out = append(out, list(s)...)
	}
	return out
}

func describeEntries(es []entry) string {
	var lines []string
	for _, e := range es {
		lines = append(lines, fmt.Sprintf("  %s  %s  (%s)", e.code, e.name, e.s.name))
	}
	return strings.Join(lines, "\n")
}

func findWaiting(all []entry, code string) (entry, error) {
	var match []entry
	for _, e := range all {
		if e.code == code {
			match = append(match, e)
		}
	}
	switch {
	case len(all) == 0:
		return entry{}, fmt.Errorf("nobody is waiting")
	case len(match) == 0:
		return entry{}, fmt.Errorf("no one is waiting with code %s\nwaiting:\n%s", code, describeEntries(all))
	case len(match) > 1:
		return entry{}, fmt.Errorf("several people are waiting with code %s; turn them away and ask your guest to rejoin:\n%s", code, describeEntries(match))
	}
	return match[0], nil
}

func admit(e entry) {
	unlock := lockShare(e.s)
	defer unlock()
	i, ok := loadInvite(e.s, e.invite)
	if !ok || i.State != "open" || i.expired() || e.s.get("closing") != "" || e.s.get("wait_"+e.hex) == "" {
		fatalf("invite is no longer accepting this request")
	}
	if i.Admission != "ask" && !i.take(e.s) {
		fatalf("invite has no admissions left")
	}
	admitLocked(e)
}

func admitLocked(e entry) {
	e.s.set("ok_"+e.hex, e.name)
	e.s.unset("wait_" + e.hex)
	e.s.must("wait-for", "-S", channel(e.hex))
}

func decline(e entry) {
	e.s.set("bye_"+e.hex, "The host declined.")
	e.s.unset("wait_" + e.hex)
	e.s.must("wait-for", "-S", channel(e.hex))
	refreshStatus(e.s)
}

func sayBye(s server, hex, fallback string) {
	msg := s.get("bye_" + hex)
	if msg == "" {
		msg = fallback
	}
	if msg != "" {
		fmt.Printf("\r\n  %s\r\n", msg)
	}
	s.run("set-option", "-gu", "@quack_bye_"+hex)
}

func cmdAllow(args []string) {
	all := allEntries(waiting)
	if len(args) == 0 {
		if len(all) == 0 {
			fatalf("nobody is waiting")
		}
		fatalf("usage: quack allow <code>\nwaiting:\n%s", describeEntries(all))
	}
	e, err := findWaiting(all, normalizeCode(args))
	if err != nil {
		fatalf("%v", err)
	}
	admit(e)
	fmt.Fprintf(os.Stderr, "let %s into %s\n", e.name, e.s.name)
}

func cmdDecline(args []string) {
	all := allEntries(waiting)
	if len(args) == 0 {
		if len(all) == 0 {
			fatalf("nobody is waiting")
		}
		fatalf("usage: quack decline <code>\nwaiting:\n%s", describeEntries(all))
	}
	e, err := findWaiting(all, normalizeCode(args))
	if err != nil {
		fatalf("%v", err)
	}
	decline(e)
	fmt.Fprintf(os.Stderr, "turned %s away from %s\n", e.name, e.s.name)
}
