package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const socketPrefix = "quack-"

var (
	tmuxOnce sync.Once
	tmuxPath string
	tmuxErr  error
)

func tmuxBin() string {
	tmuxOnce.Do(func() {
		tmuxPath, tmuxErr = exec.LookPath("tmux")
		if tmuxErr != nil {
			return
		}
		tmuxPath, tmuxErr = filepath.Abs(tmuxPath)
	})
	if tmuxErr != nil {
		fatalf("tmux not found: %v", tmuxErr)
	}
	return tmuxPath
}

func quackBin() string {
	p, err := os.Executable()
	if err != nil {
		fatalf("locating quack binary: %v", err)
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		fatalf("locating quack binary: %v", err)
	}
	return p
}

func cleanEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") {
			continue
		}
		env = append(env, e)
	}
	return env
}

type server struct{ name string }

func (s server) socket() string { return socketPrefix + s.name }

func (s server) argv(args ...string) []string {
	return append([]string{tmuxBin(), "-L", s.socket(), "-f", configPath()}, args...)
}

func (s server) cmd(args ...string) *exec.Cmd {
	a := s.argv(args...)
	c := exec.Command(a[0], a[1:]...)
	c.Env = cleanEnv()
	return c
}

func (s server) run(args ...string) (string, error) {
	c := s.cmd(args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (s server) must(args ...string) string {
	out, err := s.run(args...)
	if err != nil {
		fatalf("%v", err)
	}
	return out
}

func (s server) alive() bool {
	return s.cmd("has-session", "-t", "=main").Run() == nil
}

func (s server) get(opt string) string {
	out, err := s.run("show-options", "-gqv", "@quack_"+opt)
	if err != nil {
		return ""
	}
	return out
}

func (s server) set(opt, val string) {
	s.must("set-option", "-g", "@quack_"+opt, val)
}

func (s server) unset(opt string) {
	s.must("set-option", "-gu", "@quack_"+opt)
}

func (s server) opts(prefix string) map[string]string {
	out := s.must("show-options", "-g")
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(k, "@quack_"+prefix) {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		}
		m[strings.TrimPrefix(k, "@quack_"+prefix)] = v
	}
	return m
}

func socketDir() string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()))
}

func servers() []server {
	matches, err := filepath.Glob(filepath.Join(socketDir(), socketPrefix+"*"))
	if err != nil {
		fatalf("%v", err)
	}
	var out []server
	for _, m := range matches {
		s := server{strings.TrimPrefix(filepath.Base(m), socketPrefix)}
		if !s.alive() {
			if stale(m) {
				os.Remove(m)
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func stale(path string) bool {
	c, err := net.Dial("unix", path)
	if err == nil {
		c.Close()
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

var versionRx = regexp.MustCompile(`(\d+)\.(\d+)`)

func tmuxVersion() (int, int) {
	out, err := exec.Command(tmuxBin(), "-V").Output()
	if err != nil {
		fatalf("tmux -V: %v", err)
	}
	m := versionRx.FindStringSubmatch(string(out))
	if m == nil {
		fatalf("unrecognised tmux version %q", strings.TrimSpace(string(out)))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major, minor
}

func configPath() string {
	return filepath.Join(os.TempDir(), "quack", "tmux.conf")
}

func writeConfig() {
	major, minor := tmuxVersion()
	if major < 3 || major == 3 && minor < 3 {
		fatalf("quack needs tmux 3.3 or newer, found %d.%d", major, minor)
	}
	tmux := tmuxBin()
	conf := []string{
		"set -g allow-passthrough on",
		"set -s set-clipboard on",
		"set -g focus-events on",
		"set -g status on",
		"set -g status-left ' 🦆 Ctrl-Q '",
		"set -g prefix None",
		"set -g prefix2 None",
		"unbind -a -T prefix",
		"unbind -a -T root",
		"set -g mouse off",
		"set -g fill-character ' '",
		"set -s extended-keys on",
		"set -g set-titles on",
		"set -g set-titles-string '🦆 #T'",
		"set -g default-terminal tmux-256color",
		"set -as terminal-features ',xterm-kitty:RGB,xterm-ghostty:RGB,xterm-256color:RGB'",
		"set -s escape-time 10",
		"set -g history-limit 50000",
		"set -g status-style 'bg=colour236,fg=colour252'",
		"set -g message-style 'bg=colour236,fg=colour252,fill=colour236'",
		"set -g status-left-length 300",
		"set -g status-right ''",
		"set -g window-status-format ''",
		"set -g window-status-current-format ''",
		fmt.Sprintf(`bind -n C-q { switch-client -T quack; run-shell -b "%s _menu '#{socket_path}' '#{client_tty}'" }`, quackBin()),
		fmt.Sprintf(`bind -T quack q run-shell -b "%s _quit '#{socket_path}' '#{client_tty}'"`, quackBin()),
		fmt.Sprintf(`set-hook -g client-attached 'run-shell -b "%s _fit #{socket_path}"'`, quackBin()),
		fmt.Sprintf(`set-hook -g client-detached 'run-shell -b "%s _detached #{socket_path}"'`, quackBin()),
		fmt.Sprintf(`set-hook -g client-resized 'run-shell -b "%s _fit #{socket_path}"'`, quackBin()),
		fmt.Sprintf(`set-hook -g session-closed 'run-shell "%s has-session -t =main 2>/dev/null || %s kill-server"'`, tmux, tmux),
	}
	if major > 3 || minor >= 5 {
		conf = append(conf, "set -s extended-keys-format csi-u")
	}
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		fatalf("%v", err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(conf, "\n")+"\n"), 0o600); err != nil {
		fatalf("%v", err)
	}
}
