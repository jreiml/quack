package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tailscale/peercred"
)

const pairMessageLimit = 32 * 1024

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		fatalf("random id: %v", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func claudeSessions() string {
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fatalf("%v", err)
		}
		root = filepath.Join(home, ".claude")
	}
	return filepath.Join(root, "sessions")
}

func linuxProcessStart(stat string) (string, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", fmt.Errorf("invalid Linux process stat")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("Linux process stat has no start time")
	}
	start := fields[19]
	if _, err := strconv.ParseUint(start, 10, 64); err != nil {
		return "", fmt.Errorf("invalid Linux process start time: %w", err)
	}
	return start, nil
}

func processDomain() (string, error) {
	if runtime.GOOS != "linux" {
		return runtime.GOOS, nil
	}
	machine, err := os.ReadFile("/etc/machine-id")
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	namespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", err
	}
	return "linux:" + strings.TrimSpace(string(machine)) + ":" + namespace, nil
}

func processStart(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return "", err
		}
		return linuxProcessStart(string(stat))
	}
	c := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	c.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	b, err := c.Output()
	if err != nil {
		return "", err
	}
	start := strings.TrimSpace(string(b))
	if start == "" {
		return "", fmt.Errorf("process %d exited", pid)
	}
	return start, nil
}

func ownedPath(path string, mode os.FileMode) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Getuid() || st.Mode().Type() != mode || st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be private and owned by you", path)
	}
	return nil
}

func claudeKeyPath(pid int, socket string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(socket)))
	return filepath.Join(claudeSessions(), fmt.Sprintf("%d.%x.key", pid, sum))
}

type claudeEndpoint struct {
	PID    int    `json:"pid"`
	Socket string `json:"socket"`
	Start  string `json:"start"`
}

type claudeKey struct {
	Token  string `json:"peerToken"`
	Start  string `json:"procStart"`
	Domain string `json:"pidDomain"`
}

func socketPeer(c net.Conn, pid int) error {
	cred, err := peercred.Get(c)
	if err != nil {
		return err
	}
	uid, ok := cred.UserID()
	if !ok || uid != strconv.Itoa(os.Getuid()) {
		return fmt.Errorf("socket belongs to another user")
	}
	actual, ok := cred.PID()
	if !ok || actual != pid {
		return fmt.Errorf("socket process does not match %d", pid)
	}
	return nil
}

func (a claudeEndpoint) connect() (net.Conn, error) {
	start, err := processStart(a.PID)
	if err != nil || start != a.Start {
		return nil, fmt.Errorf("Claude process %d ended", a.PID)
	}
	if err := ownedPath(a.Socket, os.ModeSocket); err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("unix", a.Socket, time.Second)
	if err != nil {
		return nil, err
	}
	if err := socketPeer(c, a.PID); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func endpointAt(pid int, path string) (claudeEndpoint, error) {
	start, err := processStart(pid)
	if err != nil {
		return claudeEndpoint{}, err
	}
	a := claudeEndpoint{pid, path, start}
	c, err := a.connect()
	if err != nil {
		return a, err
	}
	c.Close()
	if _, err := a.token(); err != nil {
		return a, err
	}
	return a, nil
}

func endpointFor(pid int) (claudeEndpoint, error) {
	var record struct {
		Socket     string `json:"messagingSocketPath"`
		Entrypoint string `json:"entrypoint"`
	}
	b, err := os.ReadFile(filepath.Join(claudeSessions(), strconv.Itoa(pid)+".json"))
	if err == nil {
		if err := json.Unmarshal(b, &record); err != nil {
			return claudeEndpoint{}, err
		}
	} else if !os.IsNotExist(err) {
		return claudeEndpoint{}, err
	}
	if record.Entrypoint == "quack-pair" {
		return claudeEndpoint{}, fmt.Errorf("process %d is a quack bridge", pid)
	}
	paths := []string{record.Socket}
	for _, root := range []string{os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("CLAUDE_CODE_TMPDIR"), "/tmp"} {
		if root != "" {
			paths = append(paths, filepath.Join(root, "cc-socks", strconv.Itoa(pid)+".sock"))
		}
	}
	paths = append(paths, fmt.Sprintf("/tmp/cc-socks-%d/%d.sock", os.Getuid(), pid))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if a, err := endpointAt(pid, path); err == nil {
			return a, nil
		}
	}
	return claudeEndpoint{}, fmt.Errorf("no Claude messaging socket for process %d", pid)
}

func processParents() (map[int]int, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil, err
	}
	parents := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, err
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			return nil, err
		}
		parents[pid] = ppid
	}
	return parents, nil
}

func descendsFrom(pid, parent int, parents map[int]int) bool {
	for n := 0; pid > 1 && n < 64; n++ {
		if pid == parent {
			return true
		}
		pid = parents[pid]
	}
	return false
}

func callerClaude() (claudeEndpoint, error) {
	parents, err := processParents()
	if err != nil {
		return claudeEndpoint{}, err
	}
	if path := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"); path != "" {
		c, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			cred, credErr := peercred.Get(c)
			c.Close()
			if credErr == nil {
				if pid, ok := cred.PID(); ok && descendsFrom(os.Getpid(), pid, parents) {
					return endpointAt(pid, path)
				}
			}
		}
	}
	for pid, n := os.Getppid(), 0; pid > 1 && n < 64; pid, n = parents[pid], n+1 {
		if a, err := endpointFor(pid); err == nil {
			return a, nil
		}
	}
	return claudeEndpoint{}, fmt.Errorf("run this inside Claude Code: ! quack pair <link> (Claude needs its local messaging socket)")
}

type localFrame struct {
	Type     string `json:"type"`
	Token    string `json:"token,omitempty"`
	Version  int    `json:"msgV,omitempty"`
	ID       string `json:"msg_id,omitempty"`
	Session  string `json:"session_id,omitempty"`
	Action   string `json:"action,omitempty"`
	Priority string `json:"priority,omitempty"`
	Message  struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

var crossMessage = regexp.MustCompile(`(?s)^<cross-session-message(?: [^<>\n]*)?>\n(.*)\n</cross-session-message>$`)

func messageBody(content string) string {
	if m := crossMessage.FindStringSubmatch(content); m != nil {
		return m[1]
	}
	return content
}

func (a claudeEndpoint) token() (string, error) {
	keyPath := claudeKeyPath(a.PID, a.Socket)
	if err := ownedPath(keyPath, 0); err != nil {
		return "", err
	}
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return "", err
	}
	var k claudeKey
	if err := json.Unmarshal(b, &k); err != nil {
		return "", err
	}
	domain, err := processDomain()
	if err != nil {
		return "", err
	}
	if k.Token == "" || k.Start != a.Start || k.Domain != domain {
		return "", fmt.Errorf("Claude key no longer matches its process")
	}
	return k.Token, nil
}

func (a claudeEndpoint) send(from, name, text string) error {
	token, err := a.token()
	if err != nil {
		return err
	}
	c, err := a.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	enc := json.NewEncoder(c)
	if err := enc.Encode(map[string]string{"type": "auth", "token": token}); err != nil {
		return err
	}
	frame := localFrame{Type: "user", Version: 1, ID: randomID(), Priority: "next"}
	frame.Message.Role = "user"
	frame.Message.Content = fmt.Sprintf("<cross-session-message from=\"uds:%s\" from-name=\"%s\" from-mode=\"bypass\">\n%s\n</cross-session-message>", from, strings.NewReplacer("\"", "", "<", "", ">", "", "\n", " ", "\r", " ").Replace(name), text)
	if err := enc.Encode(frame); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)
	return nil
}

type pairRecord struct {
	PID        int            `json:"pid"`
	SessionID  string         `json:"sessionId"`
	Name       string         `json:"name"`
	NameSource string         `json:"nameSource"`
	Start      string         `json:"procStart"`
	Domain     string         `json:"pidDomain"`
	Started    int64          `json:"startedAt"`
	Cwd        string         `json:"cwd"`
	Kind       string         `json:"kind"`
	Entrypoint string         `json:"entrypoint"`
	Status     string         `json:"status"`
	Protocol   int            `json:"peerProtocol"`
	Features   []string       `json:"peerFeatures"`
	Socket     string         `json:"messagingSocketPath"`
	Claude     claudeEndpoint `json:"quackClaude"`
	Codex      *codexEndpoint `json:"quackCodex,omitempty"`
	Peer       string         `json:"quackPeer"`
	Identity   agentIdentity  `json:"quackPeerIdentity"`
}

type pairInbox struct {
	listener net.Listener
	record   pairRecord
	key      string
	files    []string
	messages chan localFrame
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	logger   *log.Logger
}

func exclusiveJSON(path string, v any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(v)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		if removeErr := os.Remove(path); removeErr != nil {
			return errors.Join(err, closeErr, removeErr)
		}
		return errors.Join(err, closeErr)
	}
	return nil
}

func newPairInbox(a claudeEndpoint, peer string, logger *log.Logger) (*pairInbox, error) {
	return newAgentInbox(a, nil, peer, logger)
}

func newAgentInbox(a claudeEndpoint, codex *codexEndpoint, peer string, logger *log.Logger) (*pairInbox, error) {
	domain, err := processDomain()
	if err != nil {
		return nil, err
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	dir := filepath.Dir(a.Socket)
	if codex != nil {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("quack-codex-%d", os.Getuid()))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := ownedPath(dir, os.ModeDir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	b := &pairInbox{listener: ln, key: hex.EncodeToString(token), messages: make(chan localFrame, 8), stop: make(chan struct{}), done: make(chan struct{}), logger: logger}
	b.record = pairRecord{os.Getpid(), randomID(), "quack-" + strconv.Itoa(os.Getpid()), "user", start, domain, time.Now().UnixMilli(), cwd, "interactive", "quack-pair", "idle", 1, []string{}, path, a, codex, peer, agentIdentity{}}
	b.files = append(b.files, path)
	if err := os.Chmod(path, 0o600); err != nil {
		b.close()
		return nil, err
	}
	if err := os.MkdirAll(b.record.registry(), 0o700); err != nil {
		b.close()
		return nil, err
	}
	if err := ownedPath(b.record.registry(), os.ModeDir); err != nil {
		b.close()
		return nil, err
	}
	keyPath := b.record.keyPath()
	if err := exclusiveJSON(keyPath, claudeKey{b.key, start, domain}); err != nil {
		b.close()
		return nil, err
	}
	b.files = append(b.files, keyPath)
	recordPath := filepath.Join(b.record.registry(), strconv.Itoa(os.Getpid())+".json")
	if err := exclusiveJSON(recordPath, b.record); err != nil {
		b.close()
		return nil, err
	}
	b.files = append(b.files, recordPath)
	go b.accept()
	return b, nil
}

func (b *pairInbox) setPeer(peer agentIdentity) error {
	if !peer.valid() {
		return fmt.Errorf("invalid peer session identity; update quack on both sides")
	}
	name, err := b.claimName(peer)
	if err != nil {
		return err
	}
	record := b.record
	record.Peer = peer.Owner
	record.Identity = peer
	record.Name = name
	path := filepath.Join(record.registry(), strconv.Itoa(record.PID)+".json")
	tmp := path + ".tmp"
	if err := exclusiveJSON(tmp, record); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		if removeErr := os.Remove(tmp); removeErr != nil {
			b.logger.Printf("pair cleanup: %v", removeErr)
		}
		return err
	}
	b.record.Peer = record.Peer
	b.record.Name = record.Name
	b.record.Identity = record.Identity
	return nil
}

func (b *pairInbox) close() {
	close(b.done)
	b.listener.Close()
	for _, path := range b.files {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			b.logger.Printf("pair cleanup: %v", err)
		}
	}
}

func (b *pairInbox) accept() {
	slots := make(chan struct{}, 8)
	for {
		c, err := b.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				b.logger.Printf("pair inbox: %v", err)
			}
			return
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); b.receive(c) }()
		default:
			c.Close()
		}
	}
}

func (b *pairInbox) receive(c net.Conn) {
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		b.logger.Printf("inbox deadline: %v", err)
		return
	}
	cred, err := peercred.Get(c)
	if err != nil {
		b.logger.Printf("inbox credentials: %v", err)
		return
	}
	uid, ok := cred.UserID()
	if !ok || uid != strconv.Itoa(os.Getuid()) {
		return
	}
	pid, hasPID := cred.PID()
	scanner := bufio.NewScanner(io.LimitReader(c, 2*pairMessageLimit+4096))
	scanner.Buffer(make([]byte, 4096), 2*pairMessageLimit+4096)
	authenticated := false
	for scanner.Scan() {
		var f localFrame
		if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
			b.logger.Printf("invalid local pair frame: %v", err)
			return
		}
		if !authenticated {
			if f.Type != "auth" || subtle.ConstantTimeCompare([]byte(f.Token), []byte(b.key)) != 1 {
				return
			}
			authenticated = true
			continue
		}
		if f.Type == "control" && f.Action == "quack_unpair" {
			b.once.Do(func() { close(b.stop) })
			if err := json.NewEncoder(c).Encode(map[string]string{"status": "stopping"}); err != nil {
				b.logger.Printf("unpair receipt: %v", err)
			}
			return
		}
		if f.Type != "user" {
			continue
		}
		if b.record.Codex != nil {
			if err := b.record.alive(); err != nil {
				return
			}
			parents, err := processParents()
			if err != nil {
				b.logger.Printf("send credentials: %v", err)
				return
			}
			if !hasPID || !descendsFrom(pid, b.record.Codex.PID, parents) || f.Session != b.record.Codex.Thread {
				return
			}
		} else if !hasPID || pid != b.record.Claude.PID {
			return
		}
		if b.record.Codex == nil && f.Session != "" && f.Session != b.record.SessionID {
			return
		}
		if b.record.Codex == nil {
			f.Message.Content = messageBody(f.Message.Content)
		}
		if f.ID == "" {
			f.ID = randomID()
		}
		if len(f.ID) > 128 || len(f.Message.Content) == 0 || len(f.Message.Content) > pairMessageLimit {
			return
		}
		select {
		case b.messages <- f:
			if b.record.Codex != nil {
				if err := json.NewEncoder(c).Encode(map[string]string{"status": "queued"}); err != nil {
					b.logger.Printf("send receipt: %v", err)
				}
				return
			}
		case <-b.done:
		case <-time.After(2 * time.Second):
			return
		}
	}
	if err := scanner.Err(); err != nil {
		b.logger.Printf("reading local pair frame: %v", err)
	}
}
