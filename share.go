package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

func startShare(s server) string {
	if s.shared() {
		if addr := s.get("addr"); addr != "" {
			return addr
		}
	}
	s.unset("addr")
	s.unset("error")
	s.set("host", displayName())
	s.must("new-session", "-d", "-s", "_serve", "--", quackBin(), "_serve", s.name)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.get("addr"); addr != "" {
			refreshStatus(s)
			return addr
		}
		if msg := s.get("error"); msg != "" {
			fatalf("sharing failed: %s", msg)
		}
		if !s.shared() {
			fatalf("sharing failed; see %s", logPath(s.name))
		}
		time.Sleep(100 * time.Millisecond)
	}
	fatalf("sharing timed out; see %s", logPath(s.name))
	return ""
}

func joinMessage(addr string) string {
	return "quack join " + addr
}

func copyToClipboard(text string) bool {
	for _, c := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", c[0], err)
			return false
		}
		return true
	}
	return false
}

func cmdShare(args []string) {
	auto, limit, expires := false, 0, defaultExpiry
	limitSet, expiresSet := false, false
	var rest []string
	for len(args) > 0 {
		flag, val, hasVal := strings.Cut(args[0], "=")
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
		case "--auto-approve":
			auto = true
			args = args[1:]
		case "--limit":
			n, err := strconv.Atoi(value())
			if err != nil || n < 1 {
				fatalf("--limit needs a number of people, 1 or more")
			}
			limit, limitSet = n, true
		case "--expires":
			d, err := time.ParseDuration(value())
			if err != nil || d <= 0 {
				fatalf("--expires needs a duration like 2h or 30m")
			}
			expires, expiresSet = d, true
		default:
			if strings.HasPrefix(flag, "-") {
				fatalf("unknown flag %q", flag)
			}
			rest = append(rest, args[0])
			args = args[1:]
		}
	}
	if (limitSet || expiresSet) && !auto {
		fatalf("--limit and --expires only work with --auto-approve")
	}
	s := target(rest, true)
	msg := joinMessage(startShare(s))
	if auto {
		setAuto(s, limit, expires)
	}
	fmt.Println(msg)
	if copyToClipboard(msg) {
		fmt.Fprintln(os.Stderr, "(copied to clipboard)")
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", s.name, describeMode(s))
}

func cmdClose(args []string) {
	s := target(args, true)
	if !s.shared() {
		fatalf("%s is not shared", s.name)
	}
	closeAuto(s)
	fmt.Fprintf(os.Stderr, "%s: %s\n", s.name, describeMode(s))
}

func cmdUnshare(args []string) {
	s := target(args, true)
	if !s.shared() {
		fatalf("%s is not shared", s.name)
	}
	unshare(s, "The host stopped sharing.")
	fmt.Fprintf(os.Stderr, "%s is no longer shared; the old link is dead\n", s.name)
}

func unshare(s server, reason string) {
	for hex := range s.opts("ok_") {
		s.unset("ok_" + hex)
		s.set("bye_"+hex, reason)
	}
	for hex := range s.opts("wait_") {
		s.set("bye_"+hex, reason)
		s.unset("wait_" + hex)
		s.must("wait-for", "-S", channel(hex))
	}
	for _, g := range guests(s) {
		if err := syscall.Kill(-g.pid, syscall.SIGHUP); err != nil && err != syscall.ESRCH {
			fatalf("hanging up %s: %v", g.name, err)
		}
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && len(s.opts("pid_")) > 0; {
		time.Sleep(50 * time.Millisecond)
	}
	s.must("kill-session", "-t", "=_serve")
	for id := range s.opts("guest_") {
		s.unset("guest_" + id)
	}
	s.run("set-option", "-gu", "@quack_auto")
	s.run("set-option", "-gu", "@quack_away")
	s.unset("addr")
	refreshStatus(s)
}

func cmdServe(args []string) {
	if len(args) != 1 {
		fatalf("usage: quack _serve <name>")
	}
	s := server{args[0]}
	logger, f := openLog(s.name)
	if err := unix.Dup2(int(f.Fd()), 2); err != nil {
		logger.Fatalf("dup2: %v", err)
	}
	srv := &tailcat.Server{Logf: logger.Printf}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	ln, err := srv.Listen(ctx, "tcp", ":22")
	cancel()
	if err != nil {
		s.set("error", err.Error())
		logger.Fatalf("listen: %v", err)
	}
	handler := srv.SSHConnHandler(tailcat.SSHOptions{Exec: []string{quackBin(), "_gate", s.name}})
	s.set("addr", string(srv.TailcatAddr()))
	logger.Printf("sharing as %s", srv.TailcatAddr())
	var mu sync.Mutex
	open := map[*watchedConn]bool{}
	go shutdownOnSignal(srv, &mu, open, logger)
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

func shutdownOnSignal(srv *tailcat.Server, mu *sync.Mutex, open map[*watchedConn]bool, logger *log.Logger) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	logger.Printf("shutting down on %v", <-sig)
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(open)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && count() > 0; {
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	for w := range open {
		w.Conn.Close()
	}
	mu.Unlock()
	time.Sleep(300 * time.Millisecond)
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

func hangUp(s server, remote string, logger *log.Logger) {
	id := connID(remote)
	pid, err := strconv.Atoi(s.get("pid_" + id))
	if err != nil {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGHUP); err != nil && err != syscall.ESRCH {
		logger.Printf("hangup %s: %v", remote, err)
	}
}
