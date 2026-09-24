package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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
	raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "join"))
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
	s    server
	hex  string
	code string
	name string
	tty  string
	pid  int
}

func waiting(s server) []entry {
	var out []entry
	for hex, v := range s.opts("wait_") {
		code, name, _ := strings.Cut(v, "|")
		out = append(out, entry{s: s, hex: hex, code: code, name: name})
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
		out = append(out, entry{s: s, hex: f[0], name: f[1], code: f[2], tty: "/dev/" + id, pid: pid})
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

func refreshStatus(s server) {
	var parts []string
	var names []string
	seen := map[string]bool{}
	for _, g := range guests(s) {
		if !seen[g.name] {
			seen[g.name] = true
			names = append(names, g.name)
		}
	}
	if len(names) > 0 {
		parts = append(parts, "👀 "+strings.Join(names, ", "))
	}
	for _, w := range waiting(s) {
		parts = append(parts, fmt.Sprintf("⏳ %s is waiting · %s · Ctrl-Q to let them in", w.name, w.code))
	}
	if len(parts) == 0 {
		s.must("set-option", "-g", "status", "off")
		return
	}
	s.must("set-option", "-g", "status-left", " "+strings.ReplaceAll(strings.Join(parts, "    "), "#", "##")+" ")
	s.must("set-option", "-g", "status", "on")
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
	hex := strings.TrimPrefix(pub, "nodekey:")
	who := cleanName(os.Getenv("SSH_ORIGINAL_COMMAND"))
	code := codeFor(pub)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	pidOpt := "@quack_pid_" + connID(os.Getenv("TAILCAT_REMOTE_ADDR"))
	s.must("set-option", "-g", pidOpt, strconv.Itoa(os.Getpid()))
	leave := func(code int) {
		s.run("set-option", "-gu", pidOpt)
		os.Exit(code)
	}

	if s.get("ok_"+hex) == "" {
		s.set("wait_"+hex, code+"|"+who)
		refreshStatus(s)
		logger.Printf("%s (%s) waiting", who, code)
		if err := notify(fmt.Sprintf("%s wants to join %s · %s · Ctrl-Q to let them in", who, s.name, code)); err != nil {
			logger.Printf("notify: %v", err)
		}
		fmt.Printf("\r\n  Waiting for the host to let you in.\r\n  Send them this code:  %s\r\n\r\n", code)
		for s.get("ok_"+hex) == "" {
			if !s.alive() {
				fmt.Printf("  The host's session ended.\r\n")
				leave(1)
			}
			if s.get("wait_"+hex) == "" {
				fmt.Printf("  The host declined.\r\n")
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
		fmt.Printf("  The host removed you.\r\n")
		leave(1)
	}
	attach := s.cmd("attach-session", "-t", "=main")
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := attach.Run(); err != nil {
		logger.Printf("%s attach: %v", who, err)
	}
	if !s.alive() {
		fmt.Printf("\r\nThe host's session ended.\r\n")
	} else if _, err := s.run("set-option", "-gu", "@quack_guest_"+id); err == nil {
		refreshStatus(s)
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
	e.s.set("ok_"+e.hex, e.name)
	e.s.unset("wait_" + e.hex)
	e.s.must("wait-for", "-S", channel(e.hex))
}

func kickHex(s server, hex string) {
	s.run("set-option", "-gu", "@quack_ok_"+hex)
	for _, g := range guests(s) {
		if g.hex == hex {
			if err := syscall.Kill(-g.pid, syscall.SIGHUP); err != nil && err != syscall.ESRCH {
				fatalf("hanging up %s: %v", g.name, err)
			}
		}
	}
	if s.get("wait_"+hex) != "" {
		s.unset("wait_" + hex)
		s.must("wait-for", "-S", channel(hex))
	}
	refreshStatus(s)
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

func cmdKick(args []string) {
	var candidates []entry
	if name := os.Getenv("QUACK_SESSION"); name != "" && (server{name}).alive() {
		s := server{name}
		candidates = append(guests(s), waiting(s)...)
	} else {
		candidates = append(allEntries(guests), allEntries(waiting)...)
	}
	who := strings.ToLower(strings.Join(args, " "))
	var match []entry
	for _, e := range candidates {
		if who == "" || e.code == normalizeCode(args) || strings.Contains(strings.ToLower(e.name), who) {
			match = append(match, e)
		}
	}
	switch {
	case len(candidates) == 0:
		fatalf("nobody to kick")
	case len(match) == 0:
		fatalf("no guest matches %q:\n%s", who, describeEntries(candidates))
	}
	hexes := map[string]bool{}
	for _, e := range match {
		hexes[e.hex] = true
	}
	if len(hexes) > 1 {
		fatalf("several people match; be more specific:\n%s", describeEntries(match))
	}
	kickHex(match[0].s, match[0].hex)
	fmt.Fprintf(os.Stderr, "kicked %s\n", match[0].name)
}
