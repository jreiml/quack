package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

func awayUntil(s server) (time.Time, bool) {
	ts, err := strconv.ParseInt(s.get("away"), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(ts, 0), true
}

func clock(t time.Time) string {
	now := time.Now()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

func setAuto(s server, limit int, d time.Duration) {
	unlock := lockShare(s)
	if limit > 0 {
		s.set("auto", strconv.Itoa(limit))
	} else {
		s.set("auto", "any")
	}
	s.set("away", strconv.FormatInt(time.Now().Add(d).Unix(), 10))
	for _, w := range waiting(s) {
		if !autoTake(s) {
			break
		}
		admit(w)
	}
	unlock()
	refreshStatus(s)
}

func autoTake(s server) bool {
	v := s.get("auto")
	if v == "" {
		return false
	}
	if until, ok := awayUntil(s); !ok || time.Now().After(until) {
		autoOff(s)
		return false
	}
	if v == "any" {
		return true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 1 {
		autoOff(s)
		return err == nil
	}
	s.set("auto", strconv.Itoa(n-1))
	return true
}

func autoOff(s server) {
	s.run("set-option", "-gu", "@quack_auto")
}

func autoAdmit(s server, hex, who string) (admitted, last bool) {
	unlock := lockShare(s)
	defer unlock()
	if !autoTake(s) {
		return false, false
	}
	s.set("ok_"+hex, who)
	return true, s.get("auto") == ""
}

func closeAuto(s server) {
	unlock := lockShare(s)
	autoOff(s)
	s.run("set-option", "-gu", "@quack_away")
	unlock()
	refreshStatus(s)
}

func modeLabel(s server) string {
	until, away := awayUntil(s)
	switch v := s.get("auto"); v {
	case "":
		if away {
			return "ask first, stays on until " + clock(until)
		}
		return "ask first"
	case "any":
		return "anyone joins until " + clock(until)
	case "1":
		return "next one joins until " + clock(until)
	default:
		return "next " + v + " join until " + clock(until)
	}
}

func describeMode(s server) string {
	until, _ := awayUntil(s)
	switch v := s.get("auto"); v {
	case "":
		return "New people need your OK."
	case "any":
		return "Anyone with the link joins until " + clock(until) + ". Send it privately."
	case "1":
		return "The next one joins without asking, until " + clock(until) + "."
	default:
		return fmt.Sprintf("The next %s join without asking, until %s.", v, clock(until))
	}
}

func expireLoop(s server, logger *log.Logger) {
	for range time.Tick(15 * time.Second) {
		until, ok := awayUntil(s)
		if !ok || time.Now().Before(until) {
			continue
		}
		closeAuto(s)
		logger.Printf("auto-approve expired")
		if err := notify("No longer letting people into " + s.name + " without asking."); err != nil {
			logger.Printf("notify: %v", err)
		}
		if strings.TrimSpace(s.must("list-clients", "-t", "=main")) != "" {
			continue
		}
		c := exec.Command(quackBin(), "unshare", s.name)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			logger.Printf("unshare: %v", err)
			continue
		}
		go c.Wait()
	}
}
