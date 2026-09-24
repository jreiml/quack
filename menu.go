package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func serverFromSocket(path string) server {
	name, ok := strings.CutPrefix(filepath.Base(path), socketPrefix)
	if !ok {
		fatalf("not a quack socket: %s", path)
	}
	return server{name}
}

func cmdMenu(args []string) {
	if len(args) != 2 {
		fatalf("usage: quack _menu <socket> <tty>")
	}
	s, tty := serverFromSocket(args[0]), args[1]
	if guestTTYs(s)[tty] {
		s.must("display-message", "-c", tty, "-d", "3000", "only the host can open the quack menu")
		return
	}
	act := func(a ...string) string {
		argv := append([]string{quackBin(), "_act", s.name, tty}, a...)
		return fmt.Sprintf(`run-shell -b "%s"`, strings.Join(argv, " "))
	}
	items := []string{"display-menu", "-c", tty, "-x", "C", "-y", "C", "-T", "#[align=centre] quack · " + s.name + " "}
	add := func(label, key, cmd string) { items = append(items, label, key, cmd) }
	if !s.shared() {
		add("Share (copy join command)", "s", act("share"))
	} else {
		add("Copy join command again", "s", act("share"))
		ws := waiting(s)
		for i, w := range ws {
			key := ""
			if i < 9 {
				key = strconv.Itoa(i + 1)
			}
			add("Let in "+w.name+" · "+w.code, key, act("allow", w.hex))
		}
		if len(ws) > 0 {
			add("", "", "")
		}
		seen := map[string]bool{}
		for _, g := range guests(s) {
			if seen[g.hex] {
				continue
			}
			seen[g.hex] = true
			add("Kick "+g.name, "", act("kick", g.hex))
		}
		for _, w := range ws {
			add("Turn away "+w.name+" ("+w.code+")", "", act("kick", w.hex))
		}
		add("Stop sharing", "u", act("unshare"))
	}
	add("", "", "")
	add("Detach", "d", act("detach"))
	s.must(items...)
}

func cmdAct(args []string) {
	if len(args) < 3 {
		fatalf("usage: quack _act <name> <tty> <action> [args]")
	}
	s, tty, action, rest := server{args[0]}, args[1], args[2], args[3:]
	say := func(msg string) {
		s.run("display-message", "-c", tty, "-d", "5000", strings.ReplaceAll(msg, "#", "##"))
	}
	onFatal = func(msg string) { say("quack: " + strings.SplitN(msg, "\n", 2)[0]) }
	switch action {
	case "share":
		msg := joinMessage(startShare(s))
		if copyToClipboard(msg) {
			say("quack: join command copied, paste it to your guest")
			return
		}
		s.must("set-buffer", "-w", msg)
		say("quack: join command copied (via terminal clipboard)")
	case "allow":
		if len(rest) != 1 {
			fatalf("allow needs a key")
		}
		for _, e := range waiting(s) {
			if e.hex == rest[0] {
				admit(e)
				say("quack: let " + e.name + " in")
				return
			}
		}
		fatalf("they stopped waiting")
	case "kick":
		if len(rest) != 1 {
			fatalf("kick needs a key")
		}
		name := ""
		for _, e := range append(guests(s), waiting(s)...) {
			if e.hex == rest[0] {
				name = e.name
			}
		}
		kickHex(s, rest[0])
		say("quack: kicked " + name)
	case "unshare":
		unshare(s)
		say("quack: stopped sharing, the link is dead")
	case "detach":
		s.must("detach-client", "-t", tty)
	default:
		fatalf("unknown action %q", action)
	}
}
