package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func copyToClipboard(text string) bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	var commands [][]string
	if runtime.GOOS == "darwin" {
		commands = append(commands, []string{"/usr/bin/pbcopy"})
	} else {
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			commands = append(commands, []string{"wl-copy"})
		}
		if os.Getenv("DISPLAY") != "" {
			commands = append(commands, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
		}
	}
	for _, args := range commands {
		if _, err := exec.LookPath(args[0]); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin = strings.NewReader(text)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.WaitDelay = 100 * time.Millisecond
		err := cmd.Run()
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v %s\n", args[0], err, strings.TrimSpace(stderr.String()))
			continue
		}
		return true
	}
	return false
}

func copyInvite(s server, tty string, i invite) bool {
	if copyToClipboard(i.command(s)) {
		return true
	}
	s.must("set-buffer", "-b", "quack-invite", "-w", "-t", tty, "--", i.command(s))
	showInvite(s, tty, i)
	return false
}

func showInvite(s server, tty string, i invite) {
	s.must("display-popup", "-c", tty, "-E", "-w", "90%", "-h", "80%", "-T", " Invite command ", "--", quackBin(), "_invite-command", s.name, i.ID)
}

func cmdInviteCommand(args []string) {
	if len(args) != 2 {
		fatalf("usage: quack _invite-command <name> <invite>")
	}
	s := server{args[0]}
	i, ok := loadInvite(s, args[1])
	if !ok || i.State != "open" || i.expired() {
		fatalf("invite is no longer accepting connections")
	}
	fmt.Printf("If your clipboard is unchanged, select and copy this command:\n\n%s\n\nPress Enter to close.\n", i.command(s))
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil && err != io.EOF {
		fatalf("reading terminal: %v", err)
	}
}
