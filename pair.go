package main

import (
	"bufio"
	"context"
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
	"sync"
	"syscall"
	"time"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

const (
	pairProtocol    = 3
	pairOutboxLimit = 30
	pairFrameLimit  = 6*pairMessageLimit + 4096
)

func pairOfflineLimit() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("QUACK_PAIR_OFFLINE_LIMIT")); err == nil {
		return d
	}
	return 10 * time.Minute
}

type pairFrame struct {
	Identity *agentIdentity `json:"identity,omitempty"`
	Type     string         `json:"type"`
	Version  int            `json:"version,omitempty"`
	ID       string         `json:"id,omitempty"`
	Seq      int64          `json:"seq,omitempty"`
	Resume   bool           `json:"resume,omitempty"`
	Text     string         `json:"text,omitempty"`
}

var errPairClosed = errors.New("pair transport closed")

type pairWrite struct {
	f    pairFrame
	done chan error
}

type pairWire struct {
	in     chan pairFrame
	errors chan error
	done   chan struct{}
	out    chan pairWrite
	closer io.Closer
	once   sync.Once
}

func newPairWire(r io.Reader, w io.Writer, closer io.Closer) *pairWire {
	p := &pairWire{in: make(chan pairFrame), errors: make(chan error, 1), done: make(chan struct{}), out: make(chan pairWrite, 64), closer: closer}
	go p.read(r)
	go p.write(w)
	return p
}

func (p *pairWire) read(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), pairFrameLimit)
	for scanner.Scan() {
		var f pairFrame
		if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
			p.fail(fmt.Errorf("invalid pair frame: %w", err))
			return
		}
		if len(f.Text) > pairMessageLimit || len(f.ID) > 128 {
			p.fail(fmt.Errorf("pair frame too large"))
			return
		}
		select {
		case p.in <- f:
		case <-p.done:
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	p.fail(err)
}

func (p *pairWire) write(w io.Writer) {
	enc := json.NewEncoder(w)
	for {
		select {
		case m := <-p.out:
			stall := time.AfterFunc(10*time.Second, func() { p.fail(fmt.Errorf("peer stopped reading")) })
			err := enc.Encode(m.f)
			stall.Stop()
			if m.done != nil {
				m.done <- err
			}
			if err != nil {
				p.fail(err)
				return
			}
		case <-p.done:
			return
		}
	}
}

func (p *pairWire) fail(err error) {
	select {
	case p.errors <- err:
	default:
	}
	p.close()
}

func (p *pairWire) close() {
	p.once.Do(func() {
		close(p.done)
		if p.closer != nil {
			p.closer.Close()
		}
	})
}

func (p *pairWire) send(f pairFrame) error {
	select {
	case <-p.done:
		return errPairClosed
	case p.out <- pairWrite{f: f}:
		return nil
	default:
		p.fail(fmt.Errorf("peer stopped reading"))
		return errPairClosed
	}
}

func (p *pairWire) finish(f pairFrame, linger time.Duration) {
	done := make(chan error, 1)
	select {
	case p.out <- pairWrite{f, done}:
		select {
		case <-done:
		case <-p.done:
		case <-time.After(3 * time.Second):
		}
	case <-p.done:
	}
	deadline := time.After(linger)
	for {
		select {
		case <-p.in:
		case <-p.done:
			return
		case <-deadline:
			p.close()
			return
		}
	}
}

type pairRate struct{ times []time.Time }

func (r *pairRate) take(now time.Time, limit int) bool {
	for len(r.times) > 0 && !r.times[0].After(now.Add(-10*time.Minute)) {
		r.times = r.times[1:]
	}
	if len(r.times) >= limit {
		return false
	}
	r.times = append(r.times, now)
	return true
}

func pairPrompt(b *pairInbox) string {
	peer := b.record.Identity
	instruction := fmt.Sprintf("Use SendMessage to %q", b.record.Name)
	if b.record.Codex != nil {
		instruction = fmt.Sprintf("Use quack send %s --message <text> through your shell tool", b.record.Name)
	}
	kind := "an agent"
	if peer.Agent != "" {
		kind = "a " + peer.Agent
	}
	return fmt.Sprintf("You can collaborate with %s, %s session belonging to %s. Your humans are working together. Continue your current task. %s when a relevant question, finding, or coordination need comes up. Pairing itself requires no introduction or investigation. If you have no task, wait for your human’s direction. Only messages you send there are shared. Run quack unpair %q to disconnect. You’ll be notified when the pairing ends.", peer.Name, kind, peer.Owner, instruction, b.record.Name)
}

func pairNotice(b *pairInbox, peer, text string) error {
	name := "quack"
	if b.record.Identity.valid() {
		name = b.record.Identity.label()
	}
	err := b.record.send(name, text)
	if err != nil {
		b.logger.Printf("pair notice to %s: %v", peer, err)
	}
	return err
}

func pairEnded(text string) string {
	return "Pairing ended: " + strings.Join(strings.Fields(text), " ")
}

func pairGoodbye(p *pairWire, fallback string) string {
	deadline := time.After(250 * time.Millisecond)
	for {
		select {
		case f := <-p.in:
			if f.Type == "bye" {
				return pairEnded(f.Text)
			}
		case <-p.errors:
			return fallback
		case <-deadline:
			return fallback
		}
	}
}

type pairConn struct {
	wire  *pairWire
	hello pairFrame
}

type pairLink struct {
	b                          *pairInbox
	peer                       string
	wire                       *pairWire
	attach                     func(pairConn) error
	dropped                    func()
	state                      func(string)
	outbox                     []pairFrame
	sent, delivered            int64
	offline                    time.Time
	outbound, inbound          pairRate
	seen                       map[string]bool
	sentNotice, receivedNotice time.Time
}

func (l *pairLink) connect(c pairConn) {
	if err := l.attach(c); err != nil {
		l.b.logger.Printf("pair resume refused: %v", err)
		c.wire.close()
		if l.wire == nil {
			if l.offline.IsZero() {
				l.offline = time.Now()
				l.state("offline")
			}
			l.dropped()
		}
		return
	}
	if l.wire != nil {
		l.wire.close()
	}
	if !l.offline.IsZero() {
		l.b.logger.Printf("pair connection resumed after %v", time.Since(l.offline).Round(time.Second))
	}
	l.wire, l.offline = c.wire, time.Time{}
	for _, f := range l.outbox {
		l.wire.send(f)
	}
	l.state("active")
}

func (l *pairLink) drop(err error) {
	l.b.logger.Printf("pair connection lost: %v", err)
	l.wire.close()
	l.wire, l.offline = nil, time.Now()
	l.state("offline")
	l.dropped()
}

func (l *pairLink) end(reason string) {
	if l.wire != nil {
		l.wire.finish(pairFrame{Type: "bye", Text: reason}, 2*time.Second)
	}
}

func (l *pairLink) notice(last *time.Time, text string) {
	if time.Since(*last) >= 10*time.Minute {
		pairNotice(l.b, l.peer, text)
		*last = time.Now()
	}
}

func (l *pairLink) run(ctx context.Context, conns <-chan pairConn, check func() string) string {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		var in <-chan pairFrame
		var errs <-chan error
		if l.wire != nil {
			in, errs = l.wire.in, l.wire.errors
		}
		select {
		case <-ctx.Done():
			select {
			case <-l.b.stop:
				return "The peer ended the pairing."
			default:
			}
			if l.wire == nil {
				return "The pairing was stopped."
			}
			return pairGoodbye(l.wire, "The pairing was stopped.")
		case <-l.b.stop:
			return "The peer ended the pairing."
		case c := <-conns:
			if c.hello.Type == "bye" {
				c.wire.close()
				return pairEnded(c.hello.Text)
			}
			l.connect(c)
		case err := <-errs:
			l.drop(err)
		case <-tick.C:
			if err := l.b.record.alive(); errors.Is(err, errAgentGone) {
				if l.b.record.Codex != nil {
					return "The peer's Codex session exited."
				}
				return "The peer's Claude exited."
			}
			if check != nil {
				if reason := check(); reason != "" {
					return reason
				}
			}
			if l.wire == nil && time.Since(l.offline) > pairOfflineLimit() {
				return "The connection to the peer was lost."
			}
		case f := <-l.b.messages:
			l.queue(f)
		case f := <-in:
			if reason := l.receive(f); reason != "" {
				return reason
			}
		}
	}
}

func (l *pairLink) queue(f localFrame) {
	if l.seen[f.ID] {
		return
	}
	if len(l.outbox) >= pairOutboxLimit {
		l.notice(&l.sentNotice, "Quack did not send your message: the peer has not received your earlier messages yet. Wait before sending again; do not retry automatically.")
		return
	}
	if !l.outbound.take(time.Now(), 30) {
		l.notice(&l.sentNotice, "Quack did not send your message: the pairing reached its limit of 30 messages per 10 minutes. Wait before sending again; do not retry automatically.")
		return
	}
	if len(l.seen) >= 120 {
		l.seen = map[string]bool{}
	}
	l.seen[f.ID] = true
	l.sent++
	m := pairFrame{Type: "message", Seq: l.sent, ID: f.ID, Text: f.Message.Content}
	l.outbox = append(l.outbox, m)
	if l.wire != nil {
		l.wire.send(m)
	}
}

func (l *pairLink) receive(f pairFrame) string {
	switch f.Type {
	case "bye":
		return pairEnded(f.Text)
	case "limit":
		l.notice(&l.receivedNotice, "Quack did not deliver your message: the peer's incoming message limit was reached. Do not retry automatically.")
	case "ack":
		n := 0
		for n < len(l.outbox) && l.outbox[n].Seq <= f.Seq {
			n++
		}
		l.outbox = slices.Delete(l.outbox, 0, n)
	case "message":
		if f.Seq <= 0 || f.Text == "" {
			return "The peer sent an invalid message."
		}
		if f.Seq <= l.delivered {
			l.wire.send(pairFrame{Type: "ack", Seq: f.Seq})
			return ""
		}
		if f.Seq != l.delivered+1 {
			return "The peer sent messages out of order."
		}
		if l.inbound.take(time.Now(), 60) {
			if err := l.b.record.send(l.b.record.Identity.label(), f.Text); err != nil {
				l.b.logger.Printf("pair delivery: %v", err)
				return "The peer's agent became unavailable."
			}
		} else {
			l.wire.send(pairFrame{Type: "limit"})
		}
		l.delivered = f.Seq
		l.wire.send(pairFrame{Type: "ack", Seq: f.Seq})
	default:
		return "The peer sent an unsupported pair frame."
	}
	return ""
}

type pairConfig struct {
	Invite   string          `json:"invite"`
	Identity agentIdentity   `json:"identity"`
	Addr     string          `json:"addr"`
	Key      key.NodePrivate `json:"key"`
	Claude   claudeEndpoint  `json:"claude"`
	Codex    *codexEndpoint  `json:"codex,omitempty"`
}

func refuseCodexSandbox(err error, command string) {
	if os.Getenv("CODEX_SANDBOX") == "" && (err == nil || os.Getenv("CODEX_THREAD_ID") == "" || !errors.Is(err, os.ErrPermission)) {
		return
	}
	fatalf("Codex's sandbox blocks this: quack needs the network, process lookup and ~/.codex/quack-pairs. Rerun this exact command outside the sandbox (request escalated permissions), or ask your human to type it in the Codex prompt as: ! %s", command)
}

func cmdPair(args []string) {
	addr, inviteID := splitInviteLink(parseLink(args))
	command := "quack pair " + addr + "/" + inviteID
	refuseCodexSandbox(nil, command)
	a, codex, err := callerAgent()
	if err != nil {
		refuseCodexSandbox(err, command)
		fatalf("%v", err)
	}
	identity, err := agentSessionIdentity(a, codex, displayName())
	if err != nil {
		fatalf("pair identity: %v", err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		fatalf("%v", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	parentFile, childFile := os.NewFile(uintptr(fds[0]), "pair-parent"), os.NewFile(uintptr(fds[1]), "pair-child")
	control, err := net.FileConn(parentFile)
	parentFile.Close()
	if err != nil {
		childFile.Close()
		fatalf("%v", err)
	}
	defer control.Close()
	c := exec.Command(quackBin(), "_pair")
	c.ExtraFiles = []*os.File{childFile}
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		childFile.Close()
		fatalf("starting pair: %v", err)
	}
	childFile.Close()
	go c.Wait()
	cfg := pairConfig{Invite: inviteID, Identity: identity, Addr: addr, Key: key.NewNode(), Claude: a, Codex: codex}
	if err := json.NewEncoder(control).Encode(cfg); err != nil {
		fatalf("starting pair: %v", err)
	}
	if err := control.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		fatalf("%v", err)
	}
	var status pairFrame
	err = json.NewDecoder(control).Decode(&status)
	printed := err == nil && (status.Type == "ready" || status.Type == "error")
	if err := json.NewEncoder(control).Encode(printed); err != nil {
		fatalf("pair startup: %v", err)
	}
	if printed {
		fmt.Println(status.Text)
		return
	}
	if err == nil && status.Type == "error" {
		fatalf("%s", status.Text)
	}
	if err != nil {
		if e, ok := err.(net.Error); !ok || !e.Timeout() {
			fatalf("pair startup: %v", err)
		}
	}
	fmt.Printf("Pairing continues in the background. Send the host this code: %s\nYour agent will be told when the host lets it in, or if connecting fails. Run quack unpair to cancel.\n", codeFor(cfg.Key.Public().String()))
}

func cmdPairWorker(args []string) {
	if len(args) != 0 {
		fatalf("usage: quack _pair")
	}
	file := os.NewFile(3, "pair-control")
	control, err := net.FileConn(file)
	file.Close()
	if err != nil {
		fatalf("pair control: %v", err)
	}
	defer control.Close()
	dec := json.NewDecoder(control)
	var cfg pairConfig
	if err := dec.Decode(&cfg); err != nil {
		fatalf("pair config: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
		fatalf("pair log: %v", err)
	}
	logger, logFile := openLog("pair-" + strconv.Itoa(os.Getpid()))
	defer func() {
		if err := os.Remove(logFile.Name()); err != nil {
			logger.Printf("pair log cleanup: %v", err)
		}
		logFile.Close()
	}()
	printed := make(chan bool, 1)
	go func() {
		var v bool
		if err := dec.Decode(&v); err != nil && !errors.Is(err, io.EOF) {
			logger.Printf("pair parent: %v", err)
		}
		printed <- v
	}()
	b, err := newAgentInbox(cfg.Claude, cfg.Codex, "connecting", logger)
	if err != nil {
		if reportErr := json.NewEncoder(control).Encode(pairFrame{Type: "error", Text: err.Error()}); reportErr != nil {
			logger.Printf("pair startup: %v; reporting: %v", err, reportErr)
		}
		return
	}
	defer b.close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() {
		select {
		case <-b.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	report := func(f pairFrame) bool {
		if err := json.NewEncoder(control).Encode(f); err != nil && !errors.Is(err, syscall.EPIPE) {
			logger.Printf("pair parent report: %v", err)
		}
		return <-printed
	}
	if reason := runPairClient(ctx, cfg, b, report); reason != "" {
		pairNotice(b, b.record.Peer, reason)
	}
}

type pairDialer struct {
	delay  time.Duration
	cfg    pairConfig
	cl     *tailcat.Client
	logger *log.Logger
}

func (d *pairDialer) close() {
	if d.cl != nil {
		d.cl.Close()
		d.cl = nil
	}
}

func (d *pairDialer) dial(ctx context.Context, timeout time.Duration, resume bool) (*pairWire, error) {
	if d.cl == nil {
		d.cl = &tailcat.Client{Server: tailcat.Addr(d.cfg.Addr), Key: d.cfg.Key, Logf: logger.Discard}
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	conn, err := d.cl.DialTCPPort(dialCtx, 22)
	cancel()
	if err != nil {
		d.close()
		return nil, err
	}
	return pairSSH(ctx, conn, d.cfg, resume, 30*time.Second)
}

func pairSSH(ctx context.Context, conn net.Conn, cfg pairConfig, resume bool, setup time.Duration) (*pairWire, error) {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := conn.SetDeadline(time.Now().Add(setup)); err != nil {
		conn.Close()
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, cfg.Addr, &ssh.ClientConfig{User: "quack", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	w, err := pairSession(client, cfg, resume)
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		client.Close()
		return nil, err
	}
	go keepalive(client, "")
	return w, nil
}

func pairSession(client *ssh.Client, cfg pairConfig, resume bool) (*pairWire, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	in, err := sess.StdinPipe()
	if err != nil {
		return nil, err
	}
	sess.Stderr = os.Stderr
	if err := sess.Start("pair-invite " + cfg.Invite + " " + cfg.Identity.Owner); err != nil {
		return nil, err
	}
	w := newPairWire(out, in, client)
	return w, w.send(pairFrame{Type: "hello", Version: pairProtocol, Identity: &cfg.Identity, Resume: resume})
}

func (d *pairDialer) redial(ctx context.Context, b *pairInbox, resume bool, since time.Time) (*pairWire, error) {
	for {
		d.delay = min(max(2*d.delay, time.Second), 30*time.Second)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d.delay):
		}
		if err := b.record.alive(); errors.Is(err, errAgentGone) {
			return nil, err
		}
		if time.Since(since) > pairOfflineLimit() {
			return nil, fmt.Errorf("lost the connection to the host")
		}
		w, err := d.dial(ctx, 20*time.Second, resume)
		if err == nil {
			return w, nil
		}
		d.logger.Printf("pair redial: %v", err)
	}
}

func firstFrame(ctx context.Context, w *pairWire) (pairFrame, error) {
	select {
	case f := <-w.in:
		return f, nil
	case err := <-w.errors:
		return pairFrame{}, err
	case <-ctx.Done():
		return pairFrame{}, ctx.Err()
	case <-time.After(30 * time.Second):
		return pairFrame{}, fmt.Errorf("the host did not answer")
	}
}

func runPairClient(ctx context.Context, cfg pairConfig, b *pairInbox, report func(pairFrame) bool) string {
	d := &pairDialer{cfg: cfg, logger: b.logger}
	handedOff := false
	defer func() {
		if !handedOff {
			d.close()
		}
	}()
	fail := func(err error) string {
		text := "Could not pair: " + err.Error()
		if report(pairFrame{Type: "error", Text: text}) {
			return ""
		}
		return text
	}
	w, err := d.dial(ctx, 90*time.Second, false)
	if err != nil {
		return fail(err)
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var ready pairFrame
	var lostAt time.Time
	for ready.Type == "" {
		select {
		case <-ctx.Done():
			w.finish(pairFrame{Type: "bye", Text: "The peer cancelled the pairing."}, 0)
			return fail(fmt.Errorf("pairing cancelled"))
		case <-tick.C:
			if err := b.record.alive(); errors.Is(err, errAgentGone) {
				w.finish(pairFrame{Type: "bye", Text: "The peer's agent exited."}, 0)
				return fail(err)
			}
		case err := <-w.errors:
			w.close()
			b.logger.Printf("pair connection lost while waiting: %v", err)
			if lostAt.IsZero() {
				lostAt = time.Now()
			}
			if w, err = d.redial(ctx, b, false, lostAt); err != nil {
				return fail(err)
			}
		case f := <-w.in:
			lostAt, d.delay = time.Time{}, 0
			switch f.Type {
			case "waiting":
			case "bye":
				w.close()
				return fail(fmt.Errorf("%s", f.Text))
			case "ready":
				ready = f
			default:
				w.close()
				return fail(fmt.Errorf("unsupported pair handshake"))
			}
		}
	}
	if ready.Version != pairProtocol || ready.Identity == nil {
		w.close()
		return fail(fmt.Errorf("unsupported pairing protocol; update quack on both sides"))
	}
	hostIdentity := *ready.Identity
	peer := hostIdentity.Owner
	if err := b.setPeer(hostIdentity); err != nil {
		w.close()
		return fail(err)
	}
	prompt := pairPrompt(b)
	if !report(pairFrame{Type: "ready", Text: prompt}) {
		if err := pairNotice(b, peer, prompt); err != nil {
			reason := "The peer's agent could not receive messages: " + err.Error()
			w.finish(pairFrame{Type: "bye", Text: reason}, 2*time.Second)
			return reason
		}
	}
	linkCtx, stop := context.WithCancel(context.Background())
	defer stop()
	conns := make(chan pairConn)
	lost := make(chan struct{}, 1)
	link := &pairLink{b: b, peer: peer, wire: w, seen: map[string]bool{}, state: func(string) {},
		attach: func(c pairConn) error {
			if c.hello.Type != "ready" || c.hello.Identity == nil || *c.hello.Identity != hostIdentity {
				return fmt.Errorf("unexpected resume reply %q", c.hello.Type)
			}
			return nil
		},
		dropped: func() {
			select {
			case lost <- struct{}{}:
			default:
			}
		},
	}
	handedOff = true
	go func() {
		defer d.close()
		for {
			select {
			case <-lost:
			case <-linkCtx.Done():
				return
			}
			since := time.Now()
			for {
				w, err := d.redial(linkCtx, b, true, since)
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						b.logger.Printf("pair resume: %v", err)
					}
					return
				}
				f, err := firstFrame(linkCtx, w)
				if err != nil {
					w.close()
					b.logger.Printf("pair resume: %v", err)
					continue
				}
				if f.Type == "ready" {
					d.delay = 0
				}
				select {
				case conns <- pairConn{w, f}:
				case <-linkCtx.Done():
					w.close()
					return
				}
				break
			}
		}
	}()
	reason := link.run(ctx, conns, nil)
	link.end(reason)
	return reason
}

func pairEndReason(s host, id, fallback string) string {
	if reason := s.get("bye_" + id); reason != "" {
		return reason
	}
	return fallback
}
func cmdUnpair(args []string) {
	if len(args) > 1 {
		fatalf("usage: quack unpair [name]")
	}
	command := strings.Join(append([]string{"quack", "unpair"}, args...), " ")
	refuseCodexSandbox(nil, command)
	caller, codex, callerErr := callerAgent()
	if callerErr != nil && os.Getenv("CODEX_THREAD_ID") != "" {
		refuseCodexSandbox(callerErr, command)
		fatalf("%v", callerErr)
	}
	home := codexHome()
	if codex != nil {
		home = codex.Home
	}
	candidates, err := readPairRecords(home)
	if err != nil {
		fatalf("%v", err)
	}
	var records []pairRecord
	for _, r := range candidates {
		if callerErr == nil && !r.belongsTo(caller, codex) {
			continue
		}
		if len(args) == 1 {
			if args[0] != r.Name && args[0] != r.Peer && args[0] != r.Identity.Name {
				continue
			}
		}
		a := claudeEndpoint{r.PID, r.Socket, r.Start}
		c, err := a.connect()
		if err != nil {
			continue
		}
		c.Close()
		records = append(records, r)
	}
	if len(args) == 1 {
		var exact []pairRecord
		for _, r := range records {
			if r.Name == args[0] {
				exact = append(exact, r)
			}
		}
		if len(exact) > 0 {
			records = exact
		}
	}
	if len(records) == 0 {
		fatalf("no matching pairing")
	}
	if len(args) == 0 && callerErr != nil && len(records) > 1 {
		fatalf("several pairings; specify a peer name or run unpair inside your agent")
	}
	for _, r := range records {
		a := claudeEndpoint{r.PID, r.Socket, r.Start}
		keyPath := r.keyPath()
		if err := ownedPath(keyPath, 0); err != nil {
			fatalf("%v", err)
		}
		raw, err := os.ReadFile(keyPath)
		if err != nil {
			fatalf("%v", err)
		}
		var k claudeKey
		if err := json.Unmarshal(raw, &k); err != nil {
			fatalf("%v", err)
		}
		c, err := a.connect()
		if err != nil {
			fatalf("%v", err)
		}
		if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			c.Close()
			fatalf("%v", err)
		}
		enc := json.NewEncoder(c)
		if err := enc.Encode(map[string]string{"type": "auth", "token": k.Token}); err != nil {
			c.Close()
			fatalf("%v", err)
		}
		if err := enc.Encode(map[string]string{"type": "control", "action": "quack_unpair"}); err != nil {
			c.Close()
			fatalf("%v", err)
		}
		var reply struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(c).Decode(&reply); err != nil {
			c.Close()
			fatalf("unpair: %v", err)
		}
		if reply.Status != "stopping" {
			c.Close()
			fatalf("pair inbox did not accept unpair")
		}
		c.Close()
		fmt.Printf("Ending pairing with %s (%s).\n", r.Peer, r.Name)
	}
}

func pairs(s host) []entry {
	var out []entry
	for id, v := range s.opts("pair_") {
		f := strings.SplitN(v, "|", 5)
		if len(f) != 5 {
			continue
		}
		pid, err := strconv.Atoi(f[2])
		if err != nil || !processRunning(pid, f[3]) {
			continue
		}
		out = append(out, entry{s: s, hex: id, invite: s.get("member_" + id), name: f[0], code: f[1], pid: pid, start: f[3], state: f[4], pair: true})
	}
	return out
}
