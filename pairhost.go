package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func pairID(pub string) string {
	sum := sha256.Sum256([]byte("pair:" + pub))
	return fmt.Sprintf("%x", sum)
}

func pairSocketDir() string { return filepath.Join(os.TempDir(), "quack", "pairs") }

func pairSocketPath(pid int) string {
	return filepath.Join(pairSocketDir(), strconv.Itoa(pid)+".sock")
}

func sweepPairSockets() {
	paths, err := filepath.Glob(filepath.Join(pairSocketDir(), "*.sock"))
	if err != nil {
		fatalf("%v", err)
	}
	for _, path := range paths {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(path), ".sock"))
		if err != nil || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			continue
		}
		c, err := net.Dial("unix", path)
		if err == nil {
			c.Close()
			continue
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				fatalf("%v", err)
			}
		}
	}
}

func pairWorker(s host, id string) (int, bool) {
	f := strings.SplitN(s.get("pair_"+id), "|", 5)
	if len(f) != 5 {
		return 0, false
	}
	pid, err := strconv.Atoi(f[2])
	return pid, err == nil && processRunning(pid, f[3])
}

func pairTombstone(s host, id string) string {
	_, reason, _ := strings.Cut(s.get("end_"+id), "|")
	return reason
}

func buryPair(s host, id, reason string) {
	now := time.Now()
	for other, v := range s.opts("end_") {
		at, _, _ := strings.Cut(v, "|")
		if t, err := strconv.ParseInt(at, 10, 64); err != nil || now.Sub(time.Unix(t, 0)) > time.Hour {
			s.unset("end_" + other)
		}
	}
	s.set("end_"+id, strconv.FormatInt(now.Unix(), 10)+"|"+reason)
}

func pairRefuse(reason string) {
	json.NewEncoder(os.Stdout).Encode(pairFrame{Type: "bye", Text: reason})
}

func pairRelay(s host, pub, who, inviteID string, logger *log.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	pidOpt := "pid_" + connID(os.Getenv("TAILCAT_REMOTE_ADDR"))
	s.set(pidOpt, "pair:"+strconv.Itoa(os.Getpid()))
	defer func() {
		if s.alive() {
			s.unset(pidOpt)
		}
	}()
	in := bufio.NewReaderSize(os.Stdin, pairFrameLimit)
	lines := make(chan []byte, 1)
	go func() {
		line, err := in.ReadSlice('\n')
		if err != nil {
			close(lines)
			return
		}
		lines <- slices.Clone(line)
	}()
	var line []byte
	select {
	case l, ok := <-lines:
		if !ok {
			return
		}
		line = l
	case <-hup:
		return
	case <-time.After(10 * time.Second):
		logger.Printf("pair relay: no hello within 10s")
		return
	}
	var hello pairFrame
	if err := json.Unmarshal(line, &hello); err != nil || hello.Type != "hello" || hello.Version != pairProtocol {
		pairRefuse("Unsupported pairing protocol; update quack on both sides.")
		return
	}
	pid, reason := pairWorkerFor(s, pairID(pub), who, inviteID, hello.Resume, logger)
	if reason != "" {
		pairRefuse(reason)
		return
	}
	conn, err := net.Dial("unix", pairSocketPath(pid))
	if err != nil {
		logger.Printf("pair relay: %v", err)
		pairRefuse("The pairing ended.")
		return
	}
	defer conn.Close()
	if _, err := conn.Write(line); err != nil {
		logger.Printf("pair relay: %v", err)
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(conn, in)
		conn.(*net.UnixConn).CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(os.Stdout, conn)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-hup:
	}
}

func pairWorkerFor(s host, id, who, inviteID string, resume bool, logger *log.Logger) (int, string) {
	unlock := lockShare(s)
	defer unlock()
	if reason := pairTombstone(s, id); reason != "" {
		return 0, reason
	}
	if pid, ok := pairWorker(s, id); ok {
		return pid, ""
	}
	if resume {
		return 0, "The pairing ended while you were disconnected."
	}
	if s.get("closing") != "" {
		return 0, "The host stopped sharing."
	}
	r, w, err := os.Pipe()
	if err != nil {
		logger.Printf("pair worker: %v", err)
		return 0, "Could not start the pairing."
	}
	defer r.Close()
	c := exec.Command(quackBin(), "_pairhost", s.hostName(), inviteID, who)
	c.ExtraFiles = []*os.File{w}
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = c.Start()
	w.Close()
	if err != nil {
		logger.Printf("pair worker: %v", err)
		return 0, "Could not start the pairing."
	}
	go c.Wait()
	if err := r.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		logger.Printf("pair worker: %v", err)
		return 0, "Could not start the pairing."
	}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		logger.Printf("pair worker did not start: %v", err)
		c.Process.Kill()
		if pid, _ := pairWorker(s, id); pid == c.Process.Pid {
			s.unset("pair_" + id)
		}
		return 0, "Could not start the pairing."
	}
	return c.Process.Pid, ""
}

type pairHost struct {
	s                            host
	id, who, invite, code, start string
	label                        string
	logger                       *log.Logger
}

func (p *pairHost) state(state string) {
	p.s.set("pair_"+p.id, p.label+"|"+p.code+"|"+strconv.Itoa(os.Getpid())+"|"+p.start+"|"+state)
}

func cmdPairHost(args []string) {
	if len(args) != 3 {
		fatalf("usage: quack _pairhost <name> <invite> <who>")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	h, ok := hostByName(args[0])
	if !ok {
		fatalf("no session named %s", args[0])
	}
	logger, file := openLog(h.hostName())
	defer file.Close()
	pub := os.Getenv("TAILCAT_PEER_KEY")
	if !strings.HasPrefix(pub, "nodekey:") {
		logger.Fatalf("pair worker: missing TAILCAT_PEER_KEY")
	}
	if err := os.MkdirAll(pairSocketDir(), 0o700); err != nil {
		logger.Fatalf("pair worker: %v", err)
	}
	if err := ownedPath(pairSocketDir(), os.ModeDir); err != nil {
		logger.Fatalf("pair worker: %v", err)
	}
	sweepPairSockets()
	path := pairSocketPath(os.Getpid())
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logger.Fatalf("pair worker: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		logger.Fatalf("pair worker: %v", err)
	}
	defer ln.Close()
	start, err := processStart(os.Getpid())
	if err != nil {
		logger.Fatalf("pair worker: %v", err)
	}
	p := &pairHost{s: h, id: pairID(pub), who: args[2], invite: args[1], code: codeFor(pub), start: start, label: args[2], logger: logger}
	p.state("waiting")
	ready := os.NewFile(3, "pair-ready")
	if _, err := ready.Write([]byte{1}); err != nil {
		ln.Close()
		unlock := lockShare(h)
		if pid, _ := pairWorker(h, p.id); pid == os.Getpid() {
			h.unset("pair_" + p.id)
		}
		unlock()
		logger.Fatalf("pair worker: %v", err)
	}
	ready.Close()
	conns := make(chan pairConn)
	go acceptPairConns(ln, conns, logger)
	p.run(ctx, conns)
}

func acceptPairConns(ln net.Listener, conns chan<- pairConn, logger *log.Logger) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				logger.Printf("pair worker: %v", err)
			}
			return
		}
		go func() {
			w := newPairWire(c, c, c)
			select {
			case f := <-w.in:
				conns <- pairConn{w, f}
			case <-w.errors:
				w.close()
			case <-time.After(10 * time.Second):
				w.close()
			}
		}()
	}
}

func (p *pairHost) end(reason string, w *pairWire) {
	p.logger.Printf("%s's agent (%s): %s", p.who, p.code, reason)
	if p.s.alive() {
		unlock := lockShare(p.s)
		buryPair(p.s, p.id, reason)
		for _, opt := range []string{"pair_", "wait_", "ok_", "bye_", "member_"} {
			p.s.unset(opt + p.id)
		}
		unlock()
		p.s.status()
	}
	if w != nil {
		w.finish(pairFrame{Type: "bye", Text: reason}, 2*time.Second)
	}
}

func (p *pairHost) run(ctx context.Context, conns chan pairConn) {
	s := p.s
	var c pairConn
	select {
	case c = <-conns:
	case <-ctx.Done():
		p.end("The pairing was stopped.", nil)
		return
	case <-time.After(10 * time.Second):
		p.end("Pair handshake timed out.", nil)
		return
	}
	hello := c.hello
	if hello.Type != "hello" || hello.Version != pairProtocol || hello.Identity == nil || !hello.Identity.valid() || hello.Identity.Owner != p.who {
		p.end("Unsupported pairing protocol; update quack on both sides.", c.wire)
		return
	}
	peer := *hello.Identity
	same := func(c pairConn) bool {
		f := c.hello
		return f.Type == "hello" && f.Version == pairProtocol && f.Identity != nil && *f.Identity == peer
	}
	a, codex, err := s.agent()
	if err != nil {
		if err := notify(fmt.Sprintf("%s's agent could not pair: %v", p.who, err)); err != nil {
			p.logger.Printf("notify: %v", err)
		}
		p.end(err.Error(), c.wire)
		return
	}
	identity, err := agentSessionIdentity(a, codex, s.get("host"))
	if err != nil {
		p.end(err.Error(), c.wire)
		return
	}
	b, err := newAgentInbox(a, codex, p.who, p.logger)
	if err != nil {
		p.end(err.Error(), c.wire)
		return
	}
	defer b.close()
	if err := b.setPeer(peer); err != nil {
		p.end(err.Error(), c.wire)
		return
	}
	onFatal = func(msg string) { b.close(); p.logger.Printf("pair: %s", msg) }
	defer func() { onFatal = nil }()
	p.label = peer.label()
	p.state("waiting")
	if err := requestAdmission(s, p.invite, "pair", p.id, p.label, p.code); err != nil {
		p.end(err.Error(), c.wire)
		return
	}
	wire := c.wire
	if s.get("ok_"+p.id) == "" {
		p.logger.Printf("%s's agent (%s) waiting", p.who, p.code)
		s.status()
		if err := notify(fmt.Sprintf("%s's agent wants to pair (code %s). Ctrl-Q to answer.", p.who, p.code)); err != nil {
			p.logger.Printf("notify: %v", err)
		}
		wire.send(pairFrame{Type: "waiting"})
	}
	var offline time.Time
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for s.get("ok_"+p.id) == "" {
		var in <-chan pairFrame
		var errs <-chan error
		if wire != nil {
			in, errs = wire.in, wire.errors
		}
		reason := ""
		select {
		case <-ctx.Done():
			reason = pairEndReason(s, p.id, "The pairing was stopped.")
		case <-b.stop:
			reason = "The host ended the pairing."
		case c := <-conns:
			if !same(c) {
				c.wire.finish(pairFrame{Type: "bye", Text: "This pairing belongs to another agent."}, 0)
				continue
			}
			if wire != nil {
				wire.close()
			}
			wire, offline = c.wire, time.Time{}
			wire.send(pairFrame{Type: "waiting"})
		case <-errs:
			wire.close()
			wire, offline = nil, time.Now()
		case <-in:
			reason = "The peer cancelled the pairing."
		case <-tick.C:
			if !s.alive() {
				reason = "The host ended the session."
			} else if s.get("wait_"+p.id) == "" && s.get("ok_"+p.id) == "" {
				reason = pairEndReason(s, p.id, "The host declined.")
			} else if err := b.record.alive(); errors.Is(err, errAgentGone) {
				reason = "The host's agent exited."
			} else if wire == nil && time.Since(offline) > pairOfflineLimit() {
				reason = "The peer stopped waiting."
			}
		}
		if reason != "" {
			p.end(reason, wire)
			return
		}
	}
	if !s.shared() {
		p.end("The host stopped sharing.", wire)
		return
	}
	ready := pairFrame{Type: "ready", Version: pairProtocol, Identity: &identity}
	link := &pairLink{b: b, peer: p.who, seen: map[string]bool{}, dropped: func() {},
		attach: func(c pairConn) error {
			if !same(c) {
				c.wire.finish(pairFrame{Type: "bye", Text: "This pairing belongs to another agent."}, 0)
				return fmt.Errorf("resume from a different agent")
			}
			return c.wire.send(ready)
		},
		state: func(state string) {
			p.state(state)
			s.status()
		},
	}
	if wire != nil {
		link.connect(pairConn{wire, hello})
	} else {
		link.offline = offline
		link.state("offline")
	}
	p.logger.Printf("%s's agent (%s) paired", p.who, p.code)
	if err := pairNotice(b, p.who, pairPrompt(b)); err != nil {
		p.end("The host's agent could not receive messages: "+err.Error(), link.wire)
		return
	}
	reason := link.run(ctx, conns, func() string {
		if !s.alive() {
			return "The host ended the session."
		}
		if s.get("ok_"+p.id) == "" || !s.shared() {
			return pairEndReason(s, p.id, "The host stopped sharing.")
		}
		if i, ok := loadInvite(s, p.invite); !ok || i.expired() {
			return "The invite expired."
		}
		return ""
	})
	if ctx.Err() != nil && !strings.HasPrefix(reason, pairEnded("")) {
		reason = pairEndReason(s, p.id, "The host stopped sharing.")
	}
	p.end(reason, link.wire)
	pairNotice(b, p.who, reason)
}
