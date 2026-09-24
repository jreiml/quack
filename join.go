package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

const keyMaxAge = 30 * 24 * time.Hour

func keyDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatalf("%v", err)
	}
	return filepath.Join(home, ".config", "quack", "keys")
}

func clientKeyPath(addr string, slot int) string {
	sum := sha256.Sum256([]byte(addr))
	name := hex.EncodeToString(sum[:8])
	if slot > 1 {
		name += fmt.Sprintf("-%d", slot)
	}
	return filepath.Join(keyDir(), name+".key")
}

func pruneKeys() {
	entries, err := os.ReadDir(keyDir())
	if err != nil {
		fatalf("%v", err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < keyMaxAge {
			continue
		}
		p := filepath.Join(keyDir(), e.Name())
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil {
			os.Remove(p)
		}
		f.Close()
	}
}

var heldKey *os.File

func clientKey(addr string) key.NodePrivate {
	if err := os.MkdirAll(keyDir(), 0o700); err != nil {
		fatalf("%v", err)
	}
	pruneKeys()
	for slot := 1; ; slot++ {
		p := clientKeyPath(addr, slot)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			fatalf("%v", err)
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == unix.EWOULDBLOCK {
			f.Close()
			continue
		} else if err != nil {
			fatalf("locking %s: %v", p, err)
		}
		heldKey = f
		now := time.Now()
		if err := os.Chtimes(p, now, now); err != nil {
			fatalf("%v", err)
		}
		b, err := io.ReadAll(f)
		if err != nil {
			fatalf("reading %s: %v", p, err)
		}
		var k key.NodePrivate
		if text := strings.TrimSpace(string(b)); text != "" {
			if err := k.UnmarshalText([]byte(text)); err != nil {
				fatalf("reading %s: %v", p, err)
			}
			return k
		}
		k = key.NewNode()
		text, err := k.MarshalText()
		if err != nil {
			fatalf("%v", err)
		}
		if _, err := f.Write(append(text, '\n')); err != nil {
			fatalf("writing %s: %v", p, err)
		}
		return k
	}
}

func displayName() string {
	if out, err := exec.Command("git", "config", "--get", "user.name").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name
		}
	}
	host, _ := os.Hostname()
	return os.Getenv("USER") + "@" + strings.Split(host, ".")[0]
}

func parseLink(args []string) string {
	for _, a := range strings.Fields(strings.Join(args, " ")) {
		a = strings.Trim(a, "`'\"<>")
		if strings.HasPrefix(a, "tc") {
			return a
		}
	}
	fatalf("usage: quack join <link>  (the tc… address from the host's message)")
	return ""
}

func cmdJoin(args []string) {
	addr := parseLink(args)
	if !isTTY() {
		fatalf("join needs a terminal")
	}
	k := clientKey(addr)
	fmt.Fprintf(os.Stderr, "connecting… your code is %s\n", codeFor(k.Public().String()))

	cl := &tailcat.Client{Server: tailcat.Addr(addr), Key: k, Logf: logger.Discard}
	defer cl.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(90*time.Second, cancel)
	conn, err := cl.DialTCPPort(ctx, 22)
	timer.Stop()
	if err != nil {
		fatalf("could not reach the host: %v", err)
	}
	u, err := user.Current()
	if err != nil {
		fatalf("%v", err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            u.Username,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	})
	if err != nil {
		fatalf("ssh: %v", err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		fatalf("ssh session: %v", err)
	}

	fd := int(os.Stdin.Fd())
	w, h, err := term.GetSize(fd)
	if err != nil {
		w, h = 80, 24
	}
	termName := os.Getenv("TERM")
	if termName == "" {
		termName = "xterm-256color"
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}
	if err := sess.RequestPty(termName, h, w, modes); err != nil {
		fatalf("pty: %v", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		fatalf("%v", err)
	}
	screen := &screenTracker{w: os.Stdout}
	sess.Stdout = screen
	sess.Stderr = os.Stderr

	old, err := term.MakeRaw(fd)
	if err != nil {
		fatalf("raw mode: %v", err)
	}
	defer term.Restore(fd, old)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if w, h, err := term.GetSize(fd); err == nil {
				sess.WindowChange(h, w)
			}
		}
	}()
	go forwardStdin(stdin)
	go keepalive(client)

	if err := sess.Start("join " + displayName()); err != nil {
		term.Restore(fd, old)
		fatalf("%v", err)
	}
	err = sess.Wait()
	if screen.alt {
		os.Stdout.WriteString("\x1b[?1049l")
	}
	os.Stdout.WriteString(terminalReset)
	term.Restore(fd, old)
	if exit, ok := err.(*ssh.ExitError); ok && exit.ExitStatus() != 0 {
		os.Exit(exit.ExitStatus())
	}
	fmt.Fprintln(os.Stderr, "\r\ndisconnected")
}

func keepalive(client *ssh.Client) {
	for range time.Tick(5 * time.Second) {
		timer := time.AfterFunc(10*time.Second, func() {
			fmt.Fprint(os.Stderr, "\r\nthe host stopped responding\r\n")
			client.Close()
		})
		_, _, err := client.SendRequest("keepalive@quack", true, nil)
		timer.Stop()
		if err != nil {
			return
		}
	}
}

const terminalReset = "\x1b[?25h\x1b[<u\x1b[>4;0m\x1b[?1004l\x1b[?2004l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[0m"

type screenTracker struct {
	w   io.Writer
	alt bool
}

func (t *screenTracker) Write(b []byte) (int, error) {
	on := bytes.LastIndex(b, []byte("\x1b[?1049h"))
	off := bytes.LastIndex(b, []byte("\x1b[?1049l"))
	if on != off {
		t.alt = on > off
	}
	return t.w.Write(b)
}

type keyKind int

const (
	keyOther keyKind = iota
	keyCtrlC
	keyCtrlD
	keyRelease
	keyNoise
)

const cancelEvery = 3 * time.Second

func classify(code, mods, event int) keyKind {
	if event == 3 {
		switch code {
		case 'c', 'd':
			return keyRelease
		}
		return keyNoise
	}
	if code >= 57441 && code <= 57452 {
		return keyNoise
	}
	m := mods - 1
	if m < 0 {
		m = 0
	}
	ctrl := m&4 != 0
	if m&^(4|64|128) != 0 {
		return keyOther
	}
	switch {
	case ctrl && code == 'c':
		return keyCtrlC
	case ctrl && code == 'd':
		return keyCtrlD
	}
	return keyOther
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func nextKey(b []byte) (keyKind, int) {
	switch b[0] {
	case 0x03:
		return keyCtrlC, 1
	case 0x04:
		return keyCtrlD, 1
	}
	if b[0] != 0x1b || len(b) < 3 || b[1] != '[' {
		return keyOther, 1
	}
	end := 2
	for end < len(b) && b[end] >= 0x30 && b[end] <= 0x3f {
		end++
	}
	if end >= len(b) {
		return keyOther, len(b)
	}
	params := strings.Split(string(b[2:end]), ";")
	switch b[end] {
	case 'u':
		code := atoiOr(strings.Split(params[0], ":")[0], -1)
		mods, event := 1, 1
		if len(params) > 1 {
			me := strings.Split(params[1], ":")
			mods = atoiOr(me[0], 1)
			if len(me) > 1 {
				event = atoiOr(me[1], 1)
			}
		}
		return classify(code, mods, event), end + 1
	case '~':
		if len(params) == 3 && params[0] == "27" {
			return classify(atoiOr(params[2], -1), atoiOr(params[1], 1), 1), end + 1
		}
	}
	return keyOther, end + 1
}

type inputFilter struct {
	lastCancel time.Time
	now        func() time.Time
}

func (f *inputFilter) feed(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		k, size := nextKey(b)
		raw := b[:size]
		b = b[size:]
		switch k {
		case keyCtrlD, keyRelease:
		case keyCtrlC:
			if f.now().Sub(f.lastCancel) >= cancelEvery {
				f.lastCancel = f.now()
				out = append(out, raw...)
			}
		default:
			out = append(out, raw...)
		}
	}
	return out
}

func forwardStdin(w io.WriteCloser) {
	buf := make([]byte, 4096)
	f := &inputFilter{now: time.Now}
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			w.Close()
			return
		}
		if _, err := w.Write(f.feed(buf[:n])); err != nil {
			return
		}
	}
}
