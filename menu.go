package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
		s.must("display-menu", "-c", tty, "-x", "C", "-y", "C", "-T", "#[align=centre] quack · guest ",
			"Leave", "q", fmt.Sprintf(`run-shell -b "%s _act %s %s leave"`, quackBin(), s.name, tty))
		return
	}
	act := func(a ...string) string {
		argv := append([]string{quackBin(), "_act", s.name, tty}, a...)
		return fmt.Sprintf(`run-shell -b "%s"`, strings.Join(argv, " "))
	}
	items := []string{"display-menu", "-c", tty, "-x", "C", "-y", "C", "-T", "#[align=centre] quack · " + s.name + " "}
	add := func(label, key, cmd string) { items = append(items, label, key, cmd) }
	auto := s.get("auto")
	until, away := awayUntil(s)
	if !s.shared() {
		add("Share, ask first", "s", act("share"))
		add("Share, next one joins", "o", act("share-auto", "1"))
		add("Share, anyone joins", "e", act("share-auto", "0"))
		add("", "", "")
		add("Detach (keeps running)", "q", act("detach"))
		add("End session", "x", act("end"))
		s.must(items...)
		return
	}
	add("Copy join link", "s", act("share"))
	ws := waiting(s)
	if len(ws) > 0 {
		add("", "", "")
	}
	for i, w := range ws {
		key := ""
		if i < 9 {
			key = strconv.Itoa(i + 1)
		}
		add("Let "+w.name+" in ("+w.code+")", key, act("allow", w.hex))
	}
	for _, w := range ws {
		add("Turn "+w.name+" away", "", act("decline", w.hex))
	}
	add("", "", "")
	add("-New people", "", "")
	mode := func(current bool, label, key, cmd string) {
		if current {
			add("-✓ "+label, "", "")
			return
		}
		add("  "+label, key, cmd)
	}
	mode(auto == "", "ask first", "m", act("close"))
	oneLabel := "next one joins"
	if auto != "" && auto != "any" {
		oneLabel = modeLabel(s)
	}
	mode(auto != "" && auto != "any", oneLabel, "o", act("share-auto", "1"))
	anyLabel := "anyone joins"
	if auto == "any" {
		anyLabel = modeLabel(s)
	}
	mode(auto == "any", anyLabel, "e", act("share-auto", "0"))
	add("", "", "")
	add("Stop sharing", "u", act("unshare"))
	if away {
		add("Detach (sharing stays on until "+clock(until)+")", "q", act("detach"))
	} else {
		add("Detach (sharing stops)", "q", act("detach"))
	}
	add("End session", "x", act("end"))
	s.must(items...)
}

func cmdAct(args []string) {
	if len(args) < 3 {
		fatalf("usage: quack _act <name> <tty> <action> [args]")
	}
	s, tty, action, rest := server{args[0]}, args[1], args[2], args[3:]
	say := func(msg string) {
		note := "note_" + filepath.Base(tty)
		s.set(note, msg)
		refreshStatus(s)
		time.Sleep(3 * time.Second)
		if s.alive() && s.get(note) == msg {
			s.unset(note)
			refreshStatus(s)
		}
	}
	onFatal = func(msg string) { say("⚠️  " + strings.SplitN(msg, "\n", 2)[0]) }
	switch action {
	case "share", "share-auto":
		msg := joinMessage(startShare(s))
		if action == "share-auto" {
			if len(rest) != 1 {
				fatalf("share-auto needs a limit")
			}
			limit, err := strconv.Atoi(rest[0])
			if err != nil {
				fatalf("bad limit %q", rest[0])
			}
			setAuto(s, limit, defaultExpiry)
		}
		if !copyToClipboard(msg) {
			s.must("set-buffer", "-w", msg)
		}
		say("🔗 Join link copied")
	case "close":
		closeAuto(s)
		say("✋ New people need your OK")
	case "allow":
		if len(rest) != 1 {
			fatalf("allow needs a key")
		}
		for _, e := range waiting(s) {
			if e.hex == rest[0] {
				admit(e)
				say("✅ Let " + e.name + " in")
				return
			}
		}
		fatalf("they stopped waiting")
	case "decline":
		if len(rest) != 1 {
			fatalf("decline needs a key")
		}
		for _, e := range waiting(s) {
			if e.hex == rest[0] {
				decline(e)
				say("👋 Turned " + e.name + " away")
				return
			}
		}
		fatalf("they stopped waiting")
	case "unshare":
		unshare(s, "The host stopped sharing.")
		say("🔒 Stopped sharing")
	case "detach":
		s.must("detach-client", "-t", tty)
	case "leave":
		for _, g := range guests(s) {
			if g.tty == tty {
				s.set("bye_"+g.hex, "You left the session.")
				s.must("detach-client", "-t", tty)
				return
			}
		}
		fatalf("only guests can leave")
	case "end":
		s.must("kill-server")
	default:
		fatalf("unknown action %q", action)
	}
}

func cmdDetached(args []string) {
	if len(args) != 1 {
		fatalf("usage: quack _detached <socket>")
	}
	s := serverFromSocket(args[0])
	if !s.alive() || !s.shared() || s.get("away") != "" || hostAttached(s) {
		return
	}
	unshare(s, "The host left, so sharing stopped.")
}

func cmdQuit(args []string) {
	if len(args) != 2 {
		fatalf("usage: quack _quit <socket> <tty>")
	}
	s, tty := serverFromSocket(args[0]), args[1]
	action := "detach"
	if guestTTYs(s)[tty] {
		action = "leave"
	}
	cmdAct([]string{s.name, tty, action})
}
