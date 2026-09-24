package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type agentIdentity struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

var agentNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}-[0-9]{6}$`)

func (a agentIdentity) valid() bool {
	id, err := hex.DecodeString(a.ID)
	return err == nil && len(id) == 32 && agentNamePattern.MatchString(a.Name) && a.Owner != "" && cleanName(a.Owner) == a.Owner
}

func (a agentIdentity) label() string { return a.Name + " (" + a.Owner + ")" }

func identityName(id [32]byte, base string) string {
	base = strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if base == "" {
		base = adjectives[int(id[8])%len(adjectives)] + "-" + animals[int(id[9])%len(animals)]
	}
	if len(base) > 32 {
		base = strings.TrimRight(base[:32], "-")
	}
	return fmt.Sprintf("%s-%06d", base, binary.BigEndian.Uint64(id[:8])%1000000)
}

func sessionIdentity(a claudeEndpoint, owner string) (agentIdentity, error) {
	token, err := a.token()
	if err != nil {
		return agentIdentity{}, err
	}
	sum := sha256.Sum256([]byte("quack-session-v1\x00" + token + "\x00" + strconv.Itoa(a.PID) + "\x00" + a.Start))
	base, err := sessionBase(a.PID)
	if err != nil {
		return agentIdentity{}, err
	}
	return agentIdentity{hex.EncodeToString(sum[:]), identityName(sum, base), cleanName(owner)}, nil
}

func sessionBase(pid int) (string, error) {
	parents, err := processParents()
	if err != nil {
		return "", err
	}
	base := ""
	for _, s := range servers() {
		panes, err := s.run("list-panes", "-t", "=main", "-F", "#{pane_pid}")
		if err != nil {
			return "", err
		}
		for _, pane := range strings.Fields(panes) {
			root, err := strconv.Atoi(pane)
			if err != nil {
				return "", err
			}
			if descendsFrom(pid, root, parents) {
				base = s.name
				break
			}
		}
		if base != "" {
			break
		}
	}
	return base, nil
}

func (b *pairInbox) claimName(peer agentIdentity) (string, error) {
	name := peer.Name
	for attempt := 0; attempt < 10; attempt++ {
		path := filepath.Join(b.record.registry(), ".quack-"+name+".claim")
		occupied, err := inboxNameTaken(name)
		if err != nil {
			return "", err
		}
		if !occupied {
			if err := exclusiveJSON(path, claudeEndpoint{b.record.PID, b.record.Socket, b.record.Start}); err == nil {
				b.files = append(b.files, path)
				return name, nil
			} else if !os.IsExist(err) {
				return "", err
			}
		}
		sum := sha256.Sum256([]byte(b.record.SessionID + strconv.Itoa(attempt)))
		name = fmt.Sprintf("%s-%06d", peer.Name, binary.BigEndian.Uint64(sum[:8])%1000000)
	}
	return "", fmt.Errorf("could not allocate an unambiguous inbox name for %s", peer.Name)
}

func inboxNameTaken(name string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(claudeSessions(), "*.json"))
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		var record struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &record) == nil && record.Name == name {
			return true, nil
		}
	}
	return false, nil
}
