package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tailscale/tailcat"
	"golang.org/x/sys/unix"
)

func (s server) shared() bool {
	return s.cmd("has-session", "-t", "=_serve").Run() == nil
}

func logPath(name string) string {
	return filepath.Join(os.TempDir(), "quack", name+".log")
}

func openLog(name string) (*log.Logger, *os.File) {
	f, err := os.OpenFile(logPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fatalf("%v", err)
	}
	return log.New(f, "", log.LstdFlags), f
}

func startShare(s host) string {
	if s.shared() {
		if addr := s.get("addr"); addr != "" {
			return addr
		}
	}
	s.unset("addr")
	s.unset("closing")
	s.unset("invites_ready")
	s.unset("error")
	s.set("host", displayName())
	s.startServe()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.get("addr"); addr != "" {
			s.status()
			return addr
		}
		if msg := s.get("error"); msg != "" {
			fatalf("sharing failed: %s", msg)
		}
		if !s.shared() {
			fatalf("sharing failed; see %s", logPath(s.hostName()))
		}
		time.Sleep(100 * time.Millisecond)
	}
	fatalf("sharing timed out; see %s", logPath(s.hostName()))
	return ""
}

type inviteFlags struct {
	ask, auto, disconnect, all bool
	limit                      int
	limitSet, expiresSet       bool
	expires                    time.Duration
	session                    []string
}

func parseInviteFlags(args []string, allowed ...string) (inviteFlags, []string) {
	var f inviteFlags
	var rest []string
	for len(args) > 0 {
		flag, val, hasVal := strings.Cut(args[0], "=")
		if !strings.HasPrefix(flag, "-") {
			rest = append(rest, args[0])
			args = args[1:]
			continue
		}
		if !slices.Contains(allowed, flag) {
			fatalf("unknown flag %q", flag)
		}
		value := func() string {
			if hasVal {
				args = args[1:]
				return val
			}
			if len(args) < 2 {
				fatalf("%s needs a value", flag)
			}
			v := args[1]
			args = args[2:]
			return v
		}
		switch flag {
		case "--ask":
			f.ask = true
			args = args[1:]
		case "--auto-approve":
			f.auto = true
			args = args[1:]
		case "--disconnect":
			f.disconnect = true
			args = args[1:]
		case "--all":
			f.all = true
			args = args[1:]
		case "-n":
			f.session = []string{value()}
		case "--limit":
			n, err := strconv.Atoi(value())
			if err != nil || n < 1 {
				fatalf("--limit needs a number of people, 1 or more")
			}
			f.limit, f.limitSet = n, true
		case "--expires":
			f.expires, f.expiresSet = inviteExpiry(value()), true
		}
	}
	if f.limitSet && !f.auto {
		fatalf("--limit only works with --auto-approve")
	}
	if f.ask && f.auto {
		fatalf("choose either --ask or --auto-approve")
	}
	return f, rest
}

const inviteUsage = `usage:
  quack invite new terminal|agent [-n session] [--auto-approve [--limit N]] [--expires 2h|never]
  quack invite ls [-n session]
  quack invite set <id> --ask | --auto-approve [--limit N] [--expires 2h|never]
  quack invite revoke <id> [--disconnect]
  quack invite revoke --all [-n session]
  quack invite copy <id>`

func cmdInvite(args []string) {
	if len(args) == 0 {
		fatalf("%s", inviteUsage)
	}
	switch args[0] {
	case "new":
		cmdInviteNew(args[1:])
	case "ls", "list":
		cmdInviteLs(args[1:])
	case "set":
		cmdInviteSet(args[1:])
	case "revoke":
		cmdInviteRevoke(args[1:])
	case "copy":
		cmdInviteCopy(args[1:])
	default:
		fatalf("unknown invite command %q\n%s", args[0], inviteUsage)
	}
}

func cmdInviteNew(args []string) {
	kinds := map[string]string{"terminal": "join", "agent": "pair"}
	if len(args) == 0 || kinds[args[0]] == "" {
		fatalf("usage: quack invite new terminal|agent [-n session] [--auto-approve [--limit N]] [--expires 2h|never]")
	}
	kind := kinds[args[0]]
	f, rest := parseInviteFlags(args[1:], "-n", "--auto-approve", "--limit", "--expires")
	if len(rest) > 0 {
		fatalf("unexpected %q; choose a session with -n", rest[0])
	}
	var h host
	if kind == "join" {
		h = target(f.session, true)
	} else {
		h = agentInviteHost(f.session)
	}
	if !f.auto && !h.attached() {
		fatalf("ask-first invites need an attached host; run quack attach %s, or use --auto-approve for a handover", h.hostName())
	}
	startShare(h)
	remaining := -1
	if f.auto {
		remaining = f.limit
		if !f.expiresSet {
			f.expires = defaultExpiry
		}
	}
	i := createInvite(h, kind, remaining, f.expires)
	h.status()
	printInvite(h, i)
}

func agentInviteHost(args []string) host {
	if len(args) > 0 || os.Getenv("QUACK_SESSION") != "" {
		return targetHost(args)
	}
	command := strings.Join(append([]string{"quack"}, os.Args[1:]...), " ")
	refuseCodexSandbox(nil, command)
	a, codex, err := callerAgent()
	if err != nil {
		if os.Getenv("CODEX_THREAD_ID") != "" {
			refuseCodexSandbox(err, command)
			fatalf("%v", err)
		}
		return target(nil, true)
	}
	if h, ok := agentHostFor(a, codex); ok {
		return h
	}
	return newAgentHost(a, codex)
}

func printInvite(h host, i invite) {
	msg := i.command(h)
	fmt.Println(msg)
	if copyToClipboard(msg) {
		fmt.Fprintln(os.Stderr, "(copied to clipboard)")
	}
	fmt.Fprintf(os.Stderr, "%s: %s · %s\n", h.hostName(), i.label(), i.ID[:6])
}

func cmdInviteLs(args []string) {
	f, rest := parseInviteFlags(args, "-n")
	if len(rest) > 0 {
		fatalf("unexpected %q; choose a session with -n", rest[0])
	}
	list := hosts()
	if len(f.session) > 0 {
		list = []host{targetHost(f.session)}
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	n := 0
	for _, h := range list {
		es := connections(h)
		for _, i := range invites(h) {
			if n == 0 {
				fmt.Fprintln(w, "ID\tSESSION\tINVITE\tCONNECTED\tEXPIRES")
			}
			expires := "never"
			if i.Expires != 0 {
				expires = clock(time.Unix(i.Expires, 0))
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", i.ID[:6], h.hostName(), i.label(), connected(es, i.ID), expires)
			n++
		}
	}
	if n == 0 {
		fmt.Fprintln(os.Stderr, "no invites; create one with: quack invite new terminal|agent")
		return
	}
	w.Flush()
}

func findInvite(args []string) (host, invite) {
	if len(args) != 1 {
		fatalf("%s", inviteUsage)
	}
	prefix := strings.ToLower(args[0])
	var hs []host
	var is []invite
	for _, h := range hosts() {
		for _, i := range invites(h) {
			if strings.HasPrefix(i.ID, prefix) {
				hs, is = append(hs, h), append(is, i)
			}
		}
	}
	switch len(is) {
	case 0:
		fatalf("no invite %s (see quack invite ls)", args[0])
	case 1:
		return hs[0], is[0]
	}
	fatalf("several invites start with %s; use more of the ID", args[0])
	return nil, invite{}
}

func cmdInviteSet(args []string) {
	f, rest := parseInviteFlags(args, "--ask", "--auto-approve", "--limit", "--expires")
	h, i := findInvite(rest)
	var remaining *int
	switch {
	case f.ask:
		if !h.attached() {
			fatalf("ask-first invites need an attached host; run quack attach %s", h.hostName())
		}
		n := -1
		remaining = &n
	case f.auto:
		remaining = &f.limit
	}
	var expiry *time.Duration
	if f.expiresSet {
		expiry = &f.expires
	}
	if remaining == nil && expiry == nil {
		fatalf("usage: quack invite set <id> --ask | --auto-approve [--limit N] [--expires 2h|never]")
	}
	changeInvite(h, i.ID, remaining, expiry)
	i, _ = loadInvite(h, i.ID)
	fmt.Fprintf(os.Stderr, "%s: %s · %s\n", h.hostName(), i.label(), i.ID[:6])
}

func cmdInviteRevoke(args []string) {
	f, rest := parseInviteFlags(args, "--disconnect", "--all", "-n")
	if f.all {
		if len(rest) > 0 || f.disconnect {
			fatalf("--all takes no invite ID and always disconnects")
		}
		h := targetHost(f.session)
		if !h.shared() {
			fatalf("%s has no invites", h.hostName())
		}
		unshare(h, "The host stopped sharing.")
		fmt.Fprintf(os.Stderr, "%s: revoked all invites; old links no longer work\n", h.hostName())
		return
	}
	if len(f.session) > 0 {
		fatalf("-n only works with --all")
	}
	h, i := findInvite(rest)
	revokeInvite(h, i.ID, f.disconnect, "The host revoked the invite and ended access.")
	h.status()
	fmt.Fprintf(os.Stderr, "%s: revoked %s\n", h.hostName(), i.ID[:6])
}

func cmdInviteCopy(args []string) {
	_, rest := parseInviteFlags(args)
	h, i := findInvite(rest)
	if i.State != "open" || i.expired() {
		fatalf("invite is no longer accepting connections")
	}
	printInvite(h, i)
}

func unshare(s host, reason string) {
	unlock := lockShare(s)
	if s.get("closing") != "" {
		unlock()
		return
	}
	s.set("closing", "1")
	unlock()
	for hex := range s.opts("ok_") {
		s.unset("ok_" + hex)
		s.set("bye_"+hex, reason)
	}
	for hex := range s.opts("wait_") {
		s.set("bye_"+hex, reason)
		s.unset("wait_" + hex)
		s.wake(hex)
	}
	for _, g := range guests(s) {
		if err := syscall.Kill(-g.pid, syscall.SIGHUP); err != nil && err != syscall.ESRCH {
			fatalf("hanging up %s: %v", g.name, err)
		}
	}
	endPairs(s, syscall.SIGHUP)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && len(s.opts("pid_")) > 0; {
		time.Sleep(50 * time.Millisecond)
	}
	s.stopServe()
	for id := range s.opts("pair_") {
		s.unset("pair_" + id)
	}
	for id := range s.opts("guest_") {
		s.unset("guest_" + id)
	}
	for _, prefix := range []string{"invite_", "member_", "gate_", "ok_", "wait_", "bye_", "end_"} {
		for id := range s.opts(prefix) {
			s.unset(prefix + id)
		}
	}
	s.unset("addr")
	s.status()
}

func cmdServe(args []string) {
	if len(args) != 1 {
		fatalf("usage: quack _serve <name>")
	}
	s, ok := hostByName(args[0])
	if !ok {
		fatalf("no session named %s", args[0])
	}
	logger, f := openLog(s.hostName())
	if err := unix.Dup2(int(f.Fd()), 2); err != nil {
		logger.Fatalf("dup2: %v", err)
	}
	notice, err := prepareHostKeyDir()
	if err != nil {
		s.set("error", err.Error())
		logger.Fatalf("%v", err)
	}
	if notice != "" {
		logger.Print(notice)
	}
	srv := &tailcat.Server{Logf: logger.Printf}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	ln, err := srv.Listen(ctx, "tcp", ":22")
	cancel()
	if err != nil {
		s.set("error", err.Error())
		logger.Fatalf("listen: %v", err)
	}
	handler := srv.SSHConnHandler(tailcat.SSHOptions{Exec: []string{quackBin(), "_gate", s.hostName()}})
	s.set("addr", string(srv.TailcatAddr()))
	logger.Printf("sharing as %s", srv.TailcatAddr())
	var mu sync.Mutex
	open := map[*watchedConn]bool{}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s.alive() {
			time.Sleep(2 * time.Second)
		}
		if a, ok := s.(agentHost); ok {
			for _, p := range pairs(a) {
				a.set("bye_"+p.hex, "The host's agent exited.")
			}
		}
		stop <- syscall.SIGTERM
	}()
	go shutdown(s, srv, stop, &mu, open, logger)
	go expireLoop(s, logger)
	for {
		c, err := ln.Accept()
		if err != nil {
			logger.Fatalf("accept: %v", err)
		}
		remote := c.RemoteAddr().String()
		w := &watchedConn{Conn: c, closed: make(chan struct{})}
		w.onClose = func() {
			hangUp(s, remote, logger)
			mu.Lock()
			delete(open, w)
			mu.Unlock()
		}
		mu.Lock()
		open[w] = true
		mu.Unlock()
		w.touch()
		go w.expire(idleTimeout)
		go handler(w)
	}
}

func shutdown(s host, srv *tailcat.Server, stop chan os.Signal, mu *sync.Mutex, open map[*watchedConn]bool, logger *log.Logger) {
	logger.Printf("shutting down on %v", <-stop)
	if s.alive() {
		endPairs(s, syscall.SIGTERM)
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(open)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && count() > 0; {
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	for w := range open {
		w.Conn.Close()
	}
	mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	if a, ok := s.(agentHost); ok {
		a.remove()
	}
	srv.Close()
	os.Exit(0)
}

const idleTimeout = 45 * time.Second

type watchedConn struct {
	net.Conn
	once     sync.Once
	closed   chan struct{}
	lastRead atomic.Int64
	onClose  func()
}

func (w *watchedConn) touch() { w.lastRead.Store(time.Now().UnixNano()) }

func (w *watchedConn) Read(b []byte) (int, error) {
	n, err := w.Conn.Read(b)
	if err != nil {
		w.once.Do(func() {
			close(w.closed)
			w.onClose()
		})
		return n, err
	}
	w.touch()
	return n, err
}

func (w *watchedConn) expire(after time.Duration) {
	t := time.NewTicker(after / 4)
	defer t.Stop()
	for {
		select {
		case <-w.closed:
			return
		case <-t.C:
			if time.Since(time.Unix(0, w.lastRead.Load())) > after {
				w.Conn.Close()
				return
			}
		}
	}
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

func connID(remote string) string { return nonAlnum.ReplaceAllString(remote, "_") }

func endPairs(s host, sig syscall.Signal) {
	var running []entry
	for _, p := range pairs(s) {
		if p.pid > 0 && processRunning(p.pid, p.start) {
			if err := syscall.Kill(p.pid, sig); err != nil && err != syscall.ESRCH {
				fatalf("ending pairing with %s: %v", p.name, err)
			}
			running = append(running, p)
		}
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && len(running) > 0; {
		time.Sleep(50 * time.Millisecond)
		running = slices.DeleteFunc(running, func(p entry) bool { return !processRunning(p.pid, p.start) })
	}
}

func hangUp(s host, remote string, logger *log.Logger) {
	id := connID(remote)
	value := s.get("pid_" + id)
	pair := strings.HasPrefix(value, "pair:")
	pid, err := strconv.Atoi(strings.TrimPrefix(value, "pair:"))
	if err != nil {
		return
	}
	if !pair {
		pid = -pid
	}
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil && err != syscall.ESRCH {
		logger.Printf("hangup %s: %v", remote, err)
	}
}
