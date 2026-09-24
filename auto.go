package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const defaultExpiry = 24 * time.Hour

func lockShare(s server) func() {
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "quack", s.name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fatalf("%v", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		fatalf("locking %s: %v", s.name, err)
	}
	return func() { f.Close() }
}

func clock(t time.Time) string {
	now := time.Now()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

func expireLoop(s server, logger *log.Logger) {
	for range time.Tick(5 * time.Second) {
		if !s.alive() {
			return
		}
		expireInvites(s)
		ask := false
		for _, i := range invites(s) {
			if i.Admission == "ask" && i.State == "open" {
				ask = true
			}
		}
		if s.get("invites_ready") == "" || hostAttached(s) || !ask && (staysAway(s) || len(connections(s)) > 0) {
			continue
		}
		c := exec.Command(quackBin(), "_expire", s.name)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			logger.Printf("ending expired share: %v", err)
			continue
		}
		if err := c.Wait(); err != nil {
			logger.Printf("ending expired share: %v", err)
		}
	}
}

func cmdExpire(args []string) {
	if len(args) != 1 {
		fatalf("usage: quack _expire <name>")
	}
	s := server{args[0]}
	if !s.alive() || !s.shared() || hostAttached(s) {
		return
	}
	cmdDetached([]string{s.socket()})
}
