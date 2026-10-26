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
	"time"
)

var errAgentRestarting = errors.New("the agent is restarting")

type codexRestart struct {
	Endpoint codexEndpoint `json:"endpoint"`
	Until    time.Time     `json:"until"`
}

func (a codexEndpoint) restartPath() string {
	return filepath.Join(a.Home, "quack-restarts", a.Thread+".json")
}

func (a codexEndpoint) saveRestart(state codexRestart) error {
	if !threadIDPattern.MatchString(a.Thread) {
		return fmt.Errorf("invalid Codex thread")
	}
	dir := filepath.Dir(a.restartPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := ownedPath(dir, os.ModeDir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".restart-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(state); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), a.restartPath())
}

func (a codexEndpoint) resumed() (codexEndpoint, error) {
	err := a.alive()
	if err == nil || !errors.Is(err, errAgentGone) {
		return a, err
	}
	if ownedPath(a.restartPath(), 0) != nil {
		return a, err
	}
	raw, readErr := os.ReadFile(a.restartPath())
	var state codexRestart
	if readErr != nil || json.Unmarshal(raw, &state) != nil || state.Endpoint.Home != a.Home || state.Endpoint.Thread != a.Thread {
		return a, err
	}
	if state.Endpoint.alive() == nil {
		return state.Endpoint, nil
	}
	if time.Now().After(state.Until) {
		return a, err
	}
	processes, findErr := exec.Command("ps", "-axo", "pid=,comm=").Output()
	if findErr != nil {
		return a, findErr
	}
	var found *codexEndpoint
	for _, line := range strings.Split(string(processes), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || filepath.Base(strings.Join(fields[1:], " ")) != "codex" {
			continue
		}
		pid, parseErr := strconv.Atoi(fields[0])
		if parseErr != nil {
			continue
		}
		next, findErr := codexAt(pid, a.Thread)
		if findErr != nil || next.Home != a.Home {
			continue
		}
		if found != nil {
			return a, fmt.Errorf("multiple processes own resumed Codex thread %s", a.Thread)
		}
		found = next
	}
	if found == nil {
		return a, errAgentRestarting
	}
	state.Endpoint = *found
	state.Until = time.Time{}
	if err := a.saveRestart(state); err != nil {
		return a, err
	}
	return *found, nil
}
