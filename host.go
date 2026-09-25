package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type host interface {
	hostName() string
	get(opt string) string
	set(opt, val string)
	unset(opt string)
	opts(prefix string) map[string]string
	alive() bool
	shared() bool
	attached() bool
	agent() (claudeEndpoint, *codexEndpoint, error)
	wake(hex string)
	status()
	startServe()
	stopServe()
}

func (s server) hostName() string { return s.name }

func (s server) attached() bool { return hostAttached(s) }

func (s server) wake(hex string) { s.must("wait-for", "-S", channel(hex)) }

func (s server) status() { refreshStatus(s) }

func (s server) startServe() {
	s.must("new-session", "-d", "-s", "_serve", "--", quackBin(), "_serve", s.name)
}

func (s server) stopServe() { s.must("kill-session", "-t", "=_serve") }

type agentHost struct{ id string }

type pinnedAgent struct {
	Claude claudeEndpoint `json:"claude"`
	Codex  *codexEndpoint `json:"codex,omitempty"`
	Cwd    string         `json:"cwd"`
}

func agentHostsDir() string { return filepath.Join(os.TempDir(), "quack", "hosts") }

func (a agentHost) dir() string { return filepath.Join(agentHostsDir(), a.id) }

func (a agentHost) hostName() string { return a.id }

func (a agentHost) get(opt string) string {
	b, err := os.ReadFile(filepath.Join(a.dir(), opt))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		fatalf("%v", err)
	}
	return string(b)
}

func (a agentHost) set(opt, val string) {
	f, err := os.CreateTemp(a.dir(), ".tmp-")
	if err != nil {
		fatalf("%v", err)
	}
	_, err = f.WriteString(val)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(a.dir(), opt))
	}
	if err != nil {
		os.Remove(f.Name())
		fatalf("%v", err)
	}
}

func (a agentHost) unset(opt string) {
	if err := os.Remove(filepath.Join(a.dir(), opt)); err != nil && !os.IsNotExist(err) {
		fatalf("%v", err)
	}
}

func (a agentHost) opts(prefix string) map[string]string {
	entries, err := os.ReadDir(a.dir())
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	if err != nil {
		fatalf("%v", err)
	}
	m := map[string]string{}
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(a.dir(), e.Name()))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			fatalf("%v", err)
		}
		m[name] = string(b)
	}
	return m
}

func (a agentHost) pinned() (pinnedAgent, error) {
	var p pinnedAgent
	raw := a.get("agent")
	if raw == "" {
		return p, fmt.Errorf("agent host %s has no agent", a.id)
	}
	return p, json.Unmarshal([]byte(raw), &p)
}

func (p pinnedAgent) pid() (int, string) {
	if p.Codex != nil {
		return p.Codex.PID, p.Codex.Start
	}
	return p.Claude.PID, p.Claude.Start
}

func (a agentHost) alive() bool {
	p, err := a.pinned()
	if err != nil {
		return false
	}
	pid, start := p.pid()
	return processRunning(pid, start)
}

func (a agentHost) agent() (claudeEndpoint, *codexEndpoint, error) {
	p, err := a.pinned()
	if err != nil {
		return claudeEndpoint{}, nil, err
	}
	if err := endpointAlive(p.Claude, p.Codex); err != nil {
		return claudeEndpoint{}, nil, err
	}
	return p.Claude, p.Codex, nil
}

func (a agentHost) shared() bool {
	rawPID, start, _ := strings.Cut(a.get("serve"), "|")
	pid, err := strconv.Atoi(rawPID)
	return err == nil && processRunning(pid, start)
}

func (a agentHost) attached() bool { return true }

func (a agentHost) wake(string) {}

func (a agentHost) status() {}

func (a agentHost) startServe() {
	c := exec.Command(quackBin(), "_serve", a.id)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		fatalf("starting server: %v", err)
	}
	start, err := processStart(c.Process.Pid)
	if err != nil {
		fatalf("starting server: %v", err)
	}
	a.set("serve", strconv.Itoa(c.Process.Pid)+"|"+start)
	if err := c.Process.Release(); err != nil {
		fatalf("%v", err)
	}
}

func (a agentHost) stopServe() {
	rawPID, start, _ := strings.Cut(a.get("serve"), "|")
	pid, err := strconv.Atoi(rawPID)
	if err != nil || !processRunning(pid, start) {
		return
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		fatalf("stopping server: %v", err)
	}
}

func (a agentHost) remove() {
	if err := os.RemoveAll(a.dir()); err != nil {
		fatalf("%v", err)
	}
}

func processRunning(pid int, start string) bool {
	if pid <= 0 {
		return false
	}
	actual, err := processStart(pid)
	return err == nil && actual == start
}

func endpointAlive(a claudeEndpoint, codex *codexEndpoint) error {
	if codex != nil {
		return codex.alive()
	}
	c, err := a.connect()
	if err != nil {
		return fmt.Errorf("%w: %w", errAgentGone, err)
	}
	return c.Close()
}

func agentHosts() []agentHost {
	entries, err := os.ReadDir(agentHostsDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		fatalf("%v", err)
	}
	var out []agentHost
	for _, e := range entries {
		a := agentHost{e.Name()}
		if err := ownedPath(a.dir(), os.ModeDir); err != nil {
			continue
		}
		if a.stale(e) {
			a.remove()
			continue
		}
		out = append(out, a)
	}
	return out
}

func (a agentHost) stale(e os.DirEntry) bool {
	if a.get("agent") != "" && !a.alive() {
		return true
	}
	st, err := e.Info()
	return !a.shared() && err == nil && time.Since(st.ModTime()) > 90*time.Second
}

func agentHostFor(a claudeEndpoint, codex *codexEndpoint) (agentHost, bool) {
	for _, h := range agentHosts() {
		p, err := h.pinned()
		if err != nil {
			continue
		}
		if codex != nil && p.Codex != nil && *p.Codex == *codex || codex == nil && p.Codex == nil && p.Claude == a {
			return h, true
		}
	}
	return agentHost{}, false
}

func newAgentHost(a claudeEndpoint, codex *codexEndpoint) agentHost {
	cwd, err := os.Getwd()
	if err != nil {
		fatalf("%v", err)
	}
	raw, err := json.Marshal(pinnedAgent{a, codex, cwd})
	if err != nil {
		fatalf("%v", err)
	}
	if err := os.MkdirAll(agentHostsDir(), 0o700); err != nil {
		fatalf("%v", err)
	}
	for {
		h := agentHost{newName()}
		err := os.Mkdir(h.dir(), 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			fatalf("%v", err)
		}
		h.set("agent", string(raw))
		h.set("created", strconv.FormatInt(time.Now().Unix(), 10))
		return h
	}
}

func hostByName(name string) (host, bool) {
	a := agentHost{name}
	if name != "" && !strings.ContainsAny(name, `/\`) && ownedPath(a.dir(), os.ModeDir) == nil {
		return a, true
	}
	s := server{name}
	return s, s.alive()
}

func hosts() []host {
	var out []host
	for _, s := range servers() {
		out = append(out, s)
	}
	for _, a := range agentHosts() {
		out = append(out, a)
	}
	return out
}
