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
	s.must("switch-client", "-c", tty, "-T", "root")
	if guestTTYs(s)[tty] {
		s.must("display-menu", "-c", tty, "-x", "C", "-y", "C", "-T", "#[align=centre] quack · guest ",
			"Leave", "q", fmt.Sprintf(`run-shell -b "%s _act %s %s leave"`, quackBin(), s.name, tty))
		return
	}
	showMenu(s, tty, "main", nil)
}

func menuAction(s server, tty string, args ...string) string {
	argv := append([]string{quackBin(), "_act", s.name, tty}, args...)
	var quoted []string
	for _, a := range argv {
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", "'\"'\"'")+"'")
	}
	return "run-shell -b " + strconv.Quote(strings.Join(quoted, " "))
}

func showMenu(s server, tty, view string, args []string) {
	act := func(a ...string) string { return menuAction(s, tty, a...) }
	items := []string{"display-menu", "-c", tty, "-x", "C", "-y", "C", "-T", "#[align=centre] quack · " + s.name + " ", "--"}
	add := func(label, key, cmd string) { items = append(items, label, key, cmd) }
	back := func(view string, args ...string) {
		add("Back", "Escape", act(append([]string{"menu", view}, args...)...))
	}
	confirm := func(label string, a ...string) string {
		return "confirm-before -p " + strconv.Quote(label+" (y/n)") + " " + strconv.Quote(act(a...))
	}
	switch view {
	case "main":
		for n, w := range waiting(s) {
			key := ""
			if n < 9 {
				key = strconv.Itoa(n + 1)
			}
			add("Let "+w.name+" in ("+w.code+")", key, act("allow", w.hex))
			if w.pair {
				add("-  Their messages reach your agent without asking", "", "")
				add("-  Agent tool permissions still apply; Claude may hold messages", "", "")
			}
			add("Decline "+w.name, "", act("decline", w.hex))
		}
		add("Invite to terminal…", "t", act("menu", "create", "join", "never"))
		add("Invite an agent…", "c", act("menu", "create", "pair", "never"))
		add("Manage access…", "m", act("menu", "access"))
		if s.shared() {
			add("Stop all access", "s", confirm("Revoke all invites and disconnect everyone?", "unshare"))
		}
		add("", "", "")
		add("Detach (keeps running)", "q", act("detach"))
		add("End session", "x", confirm("End this session?", "end"))
	case "create":
		if len(args) != 2 {
			fatalf("create menu needs kind and expiry")
		}
		kind, expiry := args[0], args[1]
		title := "Invite to terminal"
		if kind == "pair" {
			title = "Invite an agent"
		}
		add("-"+title, "", "")
		add("Copy · ask before admitting", "a", act("invite-create", kind, "ask", expiry))
		add("Copy · allow one connection", "1", act("invite-create", kind, "1", expiry))
		add("Copy · allow anyone", "e", act("invite-create", kind, "any", expiry))
		add("Expiry: "+expiry+"…", "x", act("menu", "expiry", "create", kind))
		back("main")
	case "access":
		es := connections(s)
		all := invites(s)
		page := 0
		if len(args) > 0 {
			var err error
			page, err = strconv.Atoi(args[0])
			if err != nil || page < 0 {
				fatalf("invalid page")
			}
		}
		page = min(page, max(0, (len(all)-1)/8))
		for n, i := range all[page*8 : min(len(all), page*8+8)] {
			count := connected(es, i.ID)
			label := i.label() + " · " + i.ID[:6]
			if count > 0 {
				label += fmt.Sprintf(" · %d connected", count)
			}
			if i.Expires != 0 && !i.expired() {
				label += " · until " + clock(time.Unix(i.Expires, 0))
			}
			add(label+"…", strconv.Itoa(n+1), act("menu", "invite", i.ID))
		}
		if page > 0 {
			add("Previous invites", "p", act("menu", "access", strconv.Itoa(page-1)))
		}
		if (page+1)*8 < len(all) {
			add("More invites", "n", act("menu", "access", strconv.Itoa(page+1)))
		}
		if len(all) == 0 {
			add("-No invites yet", "", "")
		}
		add("", "", "")
		add("Stop all agent messaging", "c", confirm("Revoke agent invites and disconnect all pairs?", "stop-access", "pair"))
		add("Stop all terminal access", "t", confirm("Revoke terminal invites and disconnect all guests?", "stop-access", "join"))
		back("main")
	case "invite":
		if len(args) != 1 {
			fatalf("invite menu needs an ID")
		}
		i, ok := loadInvite(s, args[0])
		if !ok {
			fatalf("invite no longer exists")
		}
		add("-"+i.label()+" · "+i.ID[:6], "", "")
		for _, e := range connections(s) {
			if e.invite == i.ID && e.state != "waiting" {
				add("Disconnect "+e.name, "", act("disconnect", e.hex))
			}
		}
		if i.State == "open" && !i.expired() {
			add("Copy command", "c", act("invite-copy", i.ID))
			add("Show command", "v", act("invite-show", i.ID))
		}
		if (i.State == "open" || i.State == "consumed") && !i.expired() {
			add("Admission…", "a", act("menu", "admission", i.ID))
			expiry := "never"
			if i.Expires != 0 {
				expiry = clock(time.Unix(i.Expires, 0))
			}
			add("Expiry: "+expiry+"…", "e", act("menu", "expiry", "invite", i.ID))
			add("Revoke invite", "r", act("invite-revoke", i.ID, "keep"))
		}
		add("Revoke and disconnect all", "x", confirm("Revoke this invite and disconnect everyone using it?", "invite-revoke", i.ID, "disconnect"))
		back("access")
	case "admission":
		if len(args) != 1 {
			fatalf("admission needs an ID")
		}
		for _, v := range []struct{ label, key, value string }{{"Ask before admitting", "a", "ask"}, {"Allow one more connection", "1", "1"}, {"Allow anyone", "e", "any"}} {
			add(v.label, v.key, act("invite-admission", args[0], v.value))
		}
		add("Allow next N…", "n", "command-prompt -N -p 'Number of admissions:' "+strconv.Quote(act("invite-admission", args[0], "%%")))
		back("invite", args[0])
	case "expiry":
		if len(args) != 2 {
			fatalf("expiry needs a destination")
		}
		for n, v := range []string{"never", "30m", "2h", "24h"} {
			add(v, strconv.Itoa(n), act("invite-expiry", args[0], args[1], v))
		}
		add("Custom duration…", "d", "command-prompt -p 'Duration (e.g. 45m):' "+strconv.Quote(act("invite-expiry", args[0], args[1], "%%")))
		if args[0] == "create" {
			back("create", args[1], "never")
		} else {
			back("invite", args[1])
		}
	default:
		fatalf("unknown menu %q", view)
	}
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
	if guestTTYs(s)[tty] && action != "leave" {
		fatalf("only the host can manage access")
	}
	onFatal = func(msg string) { say("⚠️  " + strings.SplitN(msg, "\n", 2)[0]) }
	switch action {
	case "menu":
		if len(rest) < 1 {
			fatalf("menu needs a view")
		}
		showMenu(s, tty, rest[0], rest[1:])
	case "invite-create":
		if len(rest) != 3 {
			fatalf("invite needs kind, admission and expiry")
		}
		remaining, expiry := inviteLimit(rest[1]), inviteExpiry(rest[2])
		startShare(s)
		i := createInvite(s, rest[0], remaining, expiry)
		if copyInvite(s, tty, i) {
			say("Command copied · " + i.label())
		}
	case "invite-copy", "invite-show":
		if len(rest) != 1 {
			fatalf("copy needs an invite")
		}
		i, ok := loadInvite(s, rest[0])
		if !ok || i.State != "open" || i.expired() {
			fatalf("invite is no longer accepting connections")
		}
		if action == "invite-show" {
			showInvite(s, tty, i)
		} else if copyInvite(s, tty, i) {
			say("Command copied · " + i.label())
		}
	case "invite-admission":
		if len(rest) != 2 {
			fatalf("admission needs an invite and limit")
		}
		n := inviteLimit(rest[1])
		changeInvite(s, rest[0], &n, nil)
		showMenu(s, tty, "invite", rest[:1])
	case "invite-expiry":
		if len(rest) != 3 {
			fatalf("expiry needs a destination and duration")
		}
		d := inviteExpiry(rest[2])
		if rest[0] == "create" {
			showMenu(s, tty, "create", []string{rest[1], rest[2]})
		} else {
			changeInvite(s, rest[1], nil, &d)
			showMenu(s, tty, "invite", rest[1:2])
		}
	case "invite-revoke":
		if len(rest) != 2 {
			fatalf("revoke needs an invite and disconnect choice")
		}
		revokeInvite(s, rest[0], rest[1] == "disconnect", "The host revoked the invite and ended access.")
		say("Invite revoked")
	case "stop-access":
		if len(rest) != 1 {
			fatalf("stop access needs a type")
		}
		stopAccess(s, rest[0], "The host stopped this type of access.")
		say("Access stopped")
	case "disconnect":
		if len(rest) != 1 {
			fatalf("disconnect needs a connection")
		}
		unlock := lockShare(s)
		for _, e := range connections(s) {
			if e.hex == rest[0] {
				disconnectEntry(e, "The host disconnected you.")
			}
		}
		unlock()
		say("Disconnected")
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
	if !s.alive() || !s.shared() || hostAttached(s) {
		return
	}
	for _, i := range invites(s) {
		if !i.Away {
			revokeInvite(s, i.ID, true, "The host left, so sharing stopped.")
		}
	}
	if !staysAway(s) {
		unshare(s, "The host left, so sharing stopped.")
	} else {
		refreshStatus(s)
	}
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
