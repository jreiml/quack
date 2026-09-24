package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

const pairProtocol = 2

type pairFrame struct {
	Identity *agentIdentity `json:"identity,omitempty"`
	Type     string         `json:"type"`
	Version  int            `json:"version,omitempty"`
	ID       string         `json:"id,omitempty"`
	Text     string         `json:"text,omitempty"`
}

type pairWire struct {
	in     chan pairFrame
	errors chan error
	done   chan struct{}
	out    io.Writer
	failed bool
}

func newPairWire(r io.Reader, w io.Writer) *pairWire {
	p := &pairWire{in: make(chan pairFrame), errors: make(chan error, 1), done: make(chan struct{}), out: w}
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 4096), 2*pairMessageLimit+4096)
		for scanner.Scan() {
			var f pairFrame
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				p.errors <- fmt.Errorf("invalid pair frame: %w", err)
				return
			}
			if len(f.Text) > pairMessageLimit || len(f.ID) > 128 {
				p.errors <- fmt.Errorf("pair frame too large")
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
		p.errors <- err
	}()
	return p
}

func (p *pairWire) send(f pairFrame) error {
	if p.failed {
		return fmt.Errorf("pair transport closed")
	}
	done := make(chan error, 1)
	go func() { done <- json.NewEncoder(p.out).Encode(f) }()
	select {
	case err := <-done:
		p.failed = err != nil
		return err
	case <-time.After(3 * time.Second):
		p.failed = true
		return fmt.Errorf("peer stopped reading")
	}
}

type pairRate struct{ times []time.Time }

func (r *pairRate) take(now time.Time) bool {
	for len(r.times) > 0 && !r.times[0].After(now.Add(-10*time.Minute)) {
		r.times = r.times[1:]
	}
	if len(r.times) >= 30 {
		return false
	}
	r.times = append(r.times, now)
	return true
}

func pairPrompt(b *pairInbox) string {
	peer := b.record.Identity
	return fmt.Sprintf("You can collaborate with %s, a Claude session belonging to %s. Your humans are working together. Continue your current task. Use SendMessage to %q when a relevant question, finding, or coordination need comes up. Pairing itself requires no introduction or investigation. If you have no task, wait for your human’s direction. Only messages you send there are shared. Run quack unpair %q to disconnect. You’ll be notified when the pairing ends.", peer.Name, peer.Owner, b.record.Name, b.record.Name)
}

func pairNotice(b *pairInbox, peer, text string) {
	name := "quack"
	if b.record.Identity.valid() {
		name = b.record.Identity.label()
	}
	if err := b.record.Claude.send(b.record.Socket, name, text); err != nil {
		b.logger.Printf("pair notice to %s: %v", peer, err)
	}
}

func bridgePair(ctx context.Context, b *pairInbox, p *pairWire, peer string, check func() string) string {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var outbound, inbound pairRate
	seen := map[string]bool{}
	var sentNotice, receivedNotice time.Time
	for {
		select {
		case <-ctx.Done():
			select {
			case <-b.stop:
				return "The peer ended the pairing."
			default:
			}
			return "The pairing was stopped."
		case <-b.stop:
			return "The peer ended the pairing."
		case <-tick.C:
			c, err := b.record.Claude.connect()
			if err != nil {
				return "The peer's Claude exited."
			}
			c.Close()
			if check != nil {
				if reason := check(); reason != "" {
					return reason
				}
			}
		case err := <-p.errors:
			if !errors.Is(err, io.EOF) {
				b.logger.Printf("pair transport: %v", err)
			}
			return "The connection to the peer ended."
		case f := <-b.messages:
			if seen["out:"+f.ID] {
				continue
			}
			if !outbound.take(time.Now()) {
				if time.Since(sentNotice) >= 10*time.Minute {
					pairNotice(b, peer, "Quack did not send your message: the pairing reached its limit of 30 messages per 10 minutes. Wait before sending again; do not retry automatically.")
					sentNotice = time.Now()
				}
				continue
			}
			if len(seen) >= 120 {
				seen = map[string]bool{}
			}
			seen["out:"+f.ID] = true
			if err := p.send(pairFrame{Type: "message", ID: f.ID, Text: f.Message.Content}); err != nil {
				b.logger.Printf("pair send: %v", err)
				return "The connection to the peer ended."
			}
		case f := <-p.in:
			switch f.Type {
			case "bye":
				return "Pairing ended: " + strings.Join(strings.Fields(f.Text), " ")
			case "limit":
				if time.Since(receivedNotice) >= 10*time.Minute {
					pairNotice(b, peer, "Quack did not deliver your message: the peer's incoming limit of 30 messages per 10 minutes was reached. Do not retry automatically.")
					receivedNotice = time.Now()
				}
			case "message":
				if f.ID == "" || f.Text == "" {
					return "The peer sent an invalid message."
				}
				if seen["in:"+f.ID] {
					continue
				}
				if !inbound.take(time.Now()) {
					if err := p.send(pairFrame{Type: "limit"}); err != nil {
						return "The connection to the peer ended."
					}
					continue
				}
				if len(seen) >= 120 {
					seen = map[string]bool{}
				}
				seen["in:"+f.ID] = true
				if err := b.record.Claude.send(b.record.Socket, b.record.Identity.label(), f.Text); err != nil {
					b.logger.Printf("pair delivery: %v", err)
					return "The peer's Claude became unavailable."
				}
			default:
				return "The peer sent an unsupported pair frame."
			}
		}
	}
}

type pairConfig struct {
	Invite   string          `json:"invite"`
	Identity agentIdentity   `json:"identity"`
	Addr     string          `json:"addr"`
	Key      key.NodePrivate `json:"key"`
	Claude   claudeEndpoint  `json:"claude"`
}

func cmdPair(args []string) {
	addr, inviteID := splitInviteLink(parseLink(args))
	a, err := callerClaude()
	if err != nil {
		fatalf("%v", err)
	}
	identity, err := sessionIdentity(a, displayName())
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
	cfg := pairConfig{Invite: inviteID, Identity: identity, Addr: addr, Key: key.NewNode(), Claude: a}
	if err := json.NewEncoder(control).Encode(cfg); err != nil {
		fatalf("starting pair: %v", err)
	}
	if err := control.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		fatalf("%v", err)
	}
	var status pairFrame
	err = json.NewDecoder(control).Decode(&status)
	printed := err == nil && status.Type == "ready"
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
	fmt.Printf("Pairing continues in the background. Send the host this code: %s\nClaude will be told when the host lets it in, or if connecting fails. Run quack unpair to cancel.\n", codeFor(cfg.Key.Public().String()))
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
	b, err := newPairInbox(cfg.Claude, "connecting", logger)
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
	reason := runPairClient(ctx, cfg, b, report)
	pairNotice(b, b.record.Peer, reason)
}

func runPairClient(ctx context.Context, cfg pairConfig, b *pairInbox, report func(pairFrame) bool) string {
	cl := &tailcat.Client{Server: tailcat.Addr(cfg.Addr), Key: cfg.Key, Logf: logger.Discard}
	defer cl.Close()
	dialCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	conn, err := cl.DialTCPPort(dialCtx, 22)
	cancel()
	fail := func(err error) string {
		text := "Could not pair: " + err.Error()
		report(pairFrame{Type: "error", Text: text})
		return text
	}
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-time.After(4 * time.Second):
				conn.Close()
			case <-b.done:
			}
		case <-b.done:
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return fail(err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, cfg.Addr, &ssh.ClientConfig{User: "quack", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		return fail(err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return fail(err)
	}
	defer sess.Close()
	out, err := sess.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		return fail(err)
	}
	sess.Stderr = os.Stderr
	if err := sess.Start("pair-invite " + cfg.Invite + " " + cfg.Identity.Owner); err != nil {
		return fail(err)
	}
	go keepalive(client)
	p := newPairWire(out, in)
	defer close(p.done)
	if err := p.send(pairFrame{Type: "hello", Version: pairProtocol, Identity: &cfg.Identity}); err != nil {
		return fail(err)
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fail(fmt.Errorf("pairing cancelled"))
		case <-tick.C:
			c, err := cfg.Claude.connect()
			if err != nil {
				return fail(err)
			}
			c.Close()
		case err := <-p.errors:
			return fail(err)
		case f := <-p.in:
			switch f.Type {
			case "waiting":
			case "bye":
				return fail(fmt.Errorf("%s", f.Text))
			case "ready":
				if f.Version != pairProtocol || f.Identity == nil {
					return fail(fmt.Errorf("unsupported pairing protocol; update quack on both sides"))
				}
				peer := f.Identity.Owner
				if err := b.setPeer(*f.Identity); err != nil {
					return fail(err)
				}
				prompt := pairPrompt(b)
				if !report(pairFrame{Type: "ready", Text: prompt}) {
					pairNotice(b, peer, prompt)
				}
				reason := bridgePair(ctx, b, p, peer, nil)
				if err := p.send(pairFrame{Type: "bye", Text: reason}); err != nil {
					b.logger.Printf("pair goodbye: %v", err)
				}
				return reason
			default:
				return fail(fmt.Errorf("unsupported pair handshake"))
			}
		}
	}
}

func pairGate(s server, pub, who, inviteID string) {
	logger, file := openLog(s.name)
	defer file.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	p := newPairWire(os.Stdin, os.Stdout)
	defer close(p.done)
	goodbye := func(reason string) {
		if err := p.send(pairFrame{Type: "bye", Text: reason}); err != nil {
			logger.Printf("pair goodbye: %v", err)
		}
	}
	var peer agentIdentity
	select {
	case f := <-p.in:
		if f.Type != "hello" || f.Version != pairProtocol || f.Identity == nil || !f.Identity.valid() || f.Identity.Owner != who {
			goodbye("Unsupported pairing protocol; update quack on both sides.")
			return
		}
		peer = *f.Identity
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
		goodbye("Pair handshake timed out.")
		return
	case <-p.errors:
		return
	}
	a, err := hostClaude(s)
	if err != nil {
		goodbye(err.Error())
		return
	}
	identity, err := sessionIdentity(a, s.get("host"))
	if err != nil {
		goodbye(err.Error())
		return
	}
	b, err := newPairInbox(a, who, logger)
	if err != nil {
		goodbye(err.Error())
		return
	}
	defer b.close()
	if err := b.setPeer(peer); err != nil {
		goodbye(err.Error())
		return
	}
	sum := sha256.Sum256([]byte("pair:" + pub + ":" + strconv.Itoa(os.Getpid())))
	id := fmt.Sprintf("%x", sum)
	code := codeFor(pub)
	pidOpt := "pid_" + connID(os.Getenv("TAILCAT_REMOTE_ADDR"))
	active := "pair_" + id
	onFatal = func(msg string) { b.close(); logger.Printf("pair: %s", msg) }
	defer func() { onFatal = nil }()
	s.set(pidOpt, "pair:"+strconv.Itoa(os.Getpid()))
	defer func() {
		if !s.alive() {
			return
		}
		for _, opt := range []string{pidOpt, active, "wait_" + id, "ok_" + id, "bye_" + id, "member_" + id} {
			s.unset(opt)
		}
		refreshStatus(s)
	}()
	s.set(active, peer.label()+"|"+code+"|"+strconv.Itoa(os.Getpid())+"|waiting")
	if err := requestAdmission(s, inviteID, "pair", id, peer.label(), code); err != nil {
		goodbye(err.Error())
		return
	}
	admitted := s.get("ok_"+id) != ""
	if !admitted {
		logger.Printf("%s's Claude (%s) waiting", who, code)
		refreshStatus(s)
		if err := notify(fmt.Sprintf("%s's Claude wants to pair (code %s). Ctrl-Q to answer.", who, code)); err != nil {
			logger.Printf("notify: %v", err)
		}
		if err := p.send(pairFrame{Type: "waiting"}); err != nil {
			logger.Printf("pair wait: %v", err)
			return
		}
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	reason := ""
	for s.get("ok_"+id) == "" {
		select {
		case <-ctx.Done():
			reason = pairEndReason(s, id, "The pairing was stopped.")
		case <-b.stop:
			reason = "The host ended the pairing."
		case <-p.errors:
			return
		case <-p.in:
			reason = "The peer cancelled the pairing."
		case <-tick.C:
			if !s.alive() {
				reason = "The host ended the session."
			} else if s.get("wait_"+id) == "" && s.get("ok_"+id) == "" {
				reason = pairEndReason(s, id, "The host declined.")
			}
			if reason == "" {
				c, err := a.connect()
				if err != nil {
					reason = "The host's Claude exited."
				} else {
					c.Close()
				}
			}
		}
		if reason != "" {
			goodbye(reason)
			return
		}
	}
	if !s.shared() {
		goodbye("The host stopped sharing.")
		return
	}
	s.set(active, peer.label()+"|"+code+"|"+strconv.Itoa(os.Getpid())+"|active")
	refreshStatus(s)
	if err := p.send(pairFrame{Type: "ready", Version: pairProtocol, Identity: &identity}); err != nil {
		logger.Printf("pair ready: %v", err)
		return
	}
	logger.Printf("%s's Claude (%s) paired", who, code)
	pairNotice(b, who, pairPrompt(b))
	reason = bridgePair(ctx, b, p, who, func() string {
		if !s.alive() {
			return "The host ended the session."
		}
		if s.get("ok_"+id) == "" || !s.shared() {
			return pairEndReason(s, id, "The host stopped sharing.")
		}
		if i, ok := loadInvite(s, inviteID); !ok || i.expired() {
			return "The invite expired."
		}
		return ""
	})
	if ctx.Err() != nil {
		reason = pairEndReason(s, id, "The host ended the session.")
	}
	logger.Printf("%s's Claude (%s): %s", who, code, reason)
	goodbye(reason)
	pairNotice(b, who, reason)
}

func pairEndReason(s server, id, fallback string) string {
	if reason := s.get("bye_" + id); reason != "" {
		return reason
	}
	return fallback
}

func cmdUnpair(args []string) {
	if len(args) > 1 {
		fatalf("usage: quack unpair [name]")
	}
	matches, err := filepath.Glob(filepath.Join(claudeSessions(), "*.json"))
	if err != nil {
		fatalf("%v", err)
	}
	var records []pairRecord
	caller, callerErr := callerClaude()
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			fatalf("%v", err)
		}
		var r pairRecord
		if err := json.Unmarshal(b, &r); err != nil {
			continue
		}
		if r.Entrypoint != "quack-pair" {
			continue
		}
		if callerErr == nil && r.Claude.PID != caller.PID {
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
		fatalf("several pairings; specify a peer name or run unpair inside Claude")
	}
	for _, r := range records {
		a := claudeEndpoint{r.PID, r.Socket, r.Start}
		keyPath := claudeKeyPath(r.PID, r.Socket)
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
		c.Close()
		fmt.Printf("Ending pairing with %s (%s).\n", r.Peer, r.Name)
	}
}

func pairs(s server) []entry {
	var out []entry
	for id, v := range s.opts("pair_") {
		f := strings.SplitN(v, "|", 4)
		if len(f) != 4 {
			continue
		}
		pid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		out = append(out, entry{s: s, hex: id, invite: s.get("member_" + id), name: f[0], code: f[1], pid: pid, state: f[3], pair: true})
	}
	return out
}
