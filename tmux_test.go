package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestServersKeepsSocketThatStillAccepts(t *testing.T) {
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(socketDir(), socketPrefix+"t-probe-fails")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	for _, s := range servers() {
		if s.name == "t-probe-fails" {
			t.Fatal("listed a server that has no main session")
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket of a listening server was removed: %v", err)
	}
}

func TestServersRemovesStaleSocket(t *testing.T) {
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(socketDir(), socketPrefix+"t-stale")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	servers()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale socket was kept: %v", err)
	}
}
