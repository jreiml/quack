package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func clientKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatalf("%v", err)
	}
	return filepath.Join(home, ".config", "quack", "client.key")
}

func clientKey() key.NodePrivate {
	p := clientKeyPath()
	var k key.NodePrivate
	b, err := os.ReadFile(p)
	if err == nil {
		if err := k.UnmarshalText([]byte(strings.TrimSpace(string(b)))); err != nil {
			fatalf("reading %s: %v", p, err)
		}
		return k
	}
	if !os.IsNotExist(err) {
		fatalf("%v", err)
	}
	k = key.NewNode()
	text, err := k.MarshalText()
	if err != nil {
		fatalf("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		fatalf("%v", err)
	}
	if err := os.WriteFile(p, append(text, '\n'), 0o600); err != nil {
		fatalf("%v", err)
	}
	return k
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
	k := clientKey()
	fmt.Fprintf(os.Stderr, "connecting… your code is %s  (Ctrl-Q q leaves)\n", codeFor(k.Public().String()))

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
	sess.Stdout = os.Stdout
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
	go forwardStdin(stdin, client)
	go keepalive(client)

	if err := sess.Start("join " + displayName()); err != nil {
		term.Restore(fd, old)
		fatalf("%v", err)
	}
	err = sess.Wait()
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

const ctrlQ = 0x11

func forwardStdin(w io.WriteCloser, client *ssh.Client) {
	buf := make([]byte, 4096)
	escape := false
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			w.Close()
			return
		}
		out := make([]byte, 0, n+1)
		for _, b := range buf[:n] {
			switch {
			case escape && b == 'q':
				client.Close()
				return
			case escape:
				out = append(out, ctrlQ, b)
				escape = false
			case b == ctrlQ:
				escape = true
			default:
				out = append(out, b)
			}
		}
		if _, err := w.Write(out); err != nil {
			return
		}
	}
}
