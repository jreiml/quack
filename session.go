package main

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

var adjectives = strings.Fields("brave calm clever cosy eager fancy gentle happy jolly kind lucky merry nimble proud quick quiet rapid shy sly snappy sunny swift tidy witty zesty bold bright crisp dapper plucky")

var animals = strings.Fields("otter fox owl lynx panda koala heron badger bison crane dingo gecko hare ibis jackal lemur llama marmot newt ocelot puffin quokka raven seal tapir vole walrus yak zebra finch")

func pickWord(words []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		fatalf("%v", err)
	}
	return words[n.Int64()]
}

func newName() string {
	for {
		name := pickWord(adjectives) + "-" + pickWord(animals)
		if _, err := os.Stat(filepath.Join(socketDir(), socketPrefix+name)); os.IsNotExist(err) {
			return name
		}
	}
}

func isTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func cmdNew(args []string) {
	name := ""
	share := false
	for len(args) > 0 && args[0] != "--" {
		switch args[0] {
		case "-n", "--name":
			if len(args) < 2 {
				fatalf("%s needs a value", args[0])
			}
			name = args[1]
			args = args[2:]
		case "-s", "--share":
			share = true
			args = args[1:]
		default:
			fatalf("unknown flag %q (put the command after --)", args[0])
		}
	}
	if len(args) > 0 {
		args = args[1:]
	}
	if len(args) == 0 {
		args = []string{"claude", "--dangerously-skip-permissions"}
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		fatalf("%v", err)
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		fatalf("%v", err)
	}
	if name == "" {
		name = newName()
	}
	s := server{name}
	if s.alive() {
		fatalf("session %s already exists", name)
	}
	dir, err := os.Getwd()
	if err != nil {
		fatalf("%v", err)
	}
	writeConfig()
	create := []string{"new-session", "-d", "-s", "main", "-c", dir, "-e", "QUACK_SESSION=" + name}
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		create = append(create, "-x", strconv.Itoa(w), "-y", strconv.Itoa(max(h-1, 1)))
	}
	create = append(create, "--", bin)
	s.must(append(create, args[1:]...)...)
	s.must("set-option", "-w", "-t", "=main:", "window-size", "manual")
	s.set("cmd", strings.Join(args, " "))
	s.set("dir", dir)
	if share {
		startShare(s)
		createInvite(s, "join", -1, 0)
	}
	if !isTTY() {
		fmt.Println(name)
		return
	}
	attach(s)
}

func attach(s server) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	c := s.cmd("attach-session", "-t", "=main")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		fatalf("tmux attach: %v", err)
	}
	wait := time.Duration(0)
	if err != nil {
		wait = time.Second
	}
	select {
	case <-hup:
		terminalClosed(s)
	case <-time.After(wait):
	}
	if exit != nil {
		os.Exit(exit.ExitCode())
	}
}

func hostAttached(s server) bool {
	guests := guestTTYs(s)
	for _, tty := range strings.Fields(s.must("list-clients", "-t", "=main", "-F", "#{client_tty}")) {
		if !guests[tty] {
			return true
		}
	}
	return false
}

func terminalClosed(s server) {
	if !s.alive() || staysAway(s) || hostAttached(s) {
		return
	}
	s.must("kill-server")
}

func cmdAttach(args []string) {
	if !isTTY() {
		fatalf("attach needs a terminal")
	}
	s := target(args, false)
	if os.Getenv("QUACK_SESSION") == s.name {
		fatalf("already inside %s", s.name)
	}
	attach(s)
}

func cmdDetach(args []string) {
	s := target(args, true)
	guests := guestTTYs(s)
	n := 0
	for _, tty := range strings.Fields(s.must("list-clients", "-t", "=main", "-F", "#{client_tty}")) {
		if guests[tty] {
			continue
		}
		s.must("detach-client", "-t", tty)
		n++
	}
	if n == 0 {
		fmt.Fprintf(os.Stderr, "%s: nothing attached\n", s.name)
	}
}

func cmdStop(args []string) {
	s := target(args, true)
	s.must("kill-server")
	fmt.Fprintf(os.Stderr, "stopped %s\n", s.name)
}

type info struct {
	s       server
	created time.Time
	cmd     string
	dir     string
	hosts   int
	guests  int
	pairs   int
	waiting int
	shared  bool
	auto    bool
}

func describe(s server) info {
	i := info{s: s, cmd: s.get("cmd"), dir: s.get("dir"), shared: s.shared()}
	for _, v := range invites(s) {
		if v.State == "open" && v.Admission != "ask" && !v.expired() {
			i.auto = true
		}
	}
	if ts, err := strconv.ParseInt(s.must("display-message", "-p", "-t", "=main:", "#{session_created}"), 10, 64); err == nil {
		i.created = time.Unix(ts, 0)
	}
	guests := guestTTYs(s)
	for _, tty := range strings.Fields(s.must("list-clients", "-t", "=main", "-F", "#{client_tty}")) {
		if guests[tty] {
			i.guests++
		} else {
			i.hosts++
		}
	}
	for _, p := range pairs(s) {
		if p.state == "active" {
			i.pairs++
		}
	}
	i.waiting = len(s.opts("wait_"))
	return i
}

func sessions() []info {
	var out []info
	for _, s := range servers() {
		out = append(out, describe(s))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].created.After(out[b].created) })
	return out
}

func tildify(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func age(t time.Time) string {
	d := time.Since(t).Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh%02d", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (i info) state() string {
	var parts []string
	if i.hosts > 0 {
		parts = append(parts, "attached")
	} else {
		parts = append(parts, "detached")
	}
	if i.shared {
		parts = append(parts, "shared")
	}
	if i.auto {
		parts = append(parts, "auto-approve")
	}
	if i.guests > 0 {
		parts = append(parts, plural(i.guests, "guest"))
	}
	if i.pairs > 0 {
		parts = append(parts, plural(i.pairs, "pair"))
	}
	if i.waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting", i.waiting))
	}
	return strings.Join(parts, " · ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func printSessions(list []info) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCMD\tDIR\tAGE\tSTATE")
	for _, i := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", i.s.name, i.cmd, tildify(i.dir), age(i.created), i.state())
	}
	w.Flush()
}

func cmdLs(args []string) {
	list := sessions()
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions; start one with: quack new")
		return
	}
	printSessions(list)
}

func target(args []string, strict bool) server {
	if len(args) > 1 {
		fatalf("too many arguments")
	}
	if len(args) == 1 {
		s := server{args[0]}
		if !s.alive() {
			fatalf("no session named %s (see quack ls)", args[0])
		}
		return s
	}
	if name := os.Getenv("QUACK_SESSION"); name != "" {
		if s := (server{name}); s.alive() {
			return s
		}
	}
	list := sessions()
	switch {
	case len(list) == 0:
		fatalf("no sessions; start one with: quack new")
	case len(list) == 1 || !strict:
		return list[0].s
	}
	if !isTTY() {
		printSessions(list)
		fatalf("several sessions; name one")
	}
	return pick(list)
}

func pick(list []info) server {
	for n, i := range list {
		fmt.Fprintf(os.Stderr, "%2d) %-18s %-10s %s  %s\n", n+1, i.s.name, i.cmd, tildify(i.dir), i.state())
	}
	fmt.Fprint(os.Stderr, "which session? ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fatalf("%v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(list) {
		fatalf("no such choice %q", strings.TrimSpace(line))
	}
	return list[n-1].s
}

func fitHost(s server) {
	guests := guestTTYs(s)
	best, w, h := int64(-1), 0, 0
	for _, line := range strings.Split(s.must("list-clients", "-t", "=main", "-F", "#{client_activity} #{client_width} #{client_height} #{client_tty}"), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 || guests[f[3]] {
			continue
		}
		act, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil || act <= best {
			continue
		}
		cw, errW := strconv.Atoi(f[1])
		ch, errH := strconv.Atoi(f[2])
		if errW != nil || errH != nil {
			continue
		}
		best, w, h = act, cw, ch
	}
	if best < 0 {
		return
	}
	if s.must("show-options", "-gv", "status") == "on" {
		h--
	}
	s.must("resize-window", "-t", "=main:", "-x", strconv.Itoa(w), "-y", strconv.Itoa(h))
}

func cmdFit(args []string) {
	if len(args) != 1 {
		fatalf("usage: quack _fit <socket>")
	}
	fitHost(serverFromSocket(args[0]))
}
