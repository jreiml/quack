package main

import "testing"

func TestGuestIDRoundTripsTTY(t *testing.T) {
	for _, tty := range []string{"/dev/pts/7", "/dev/ttys007"} {
		if got := guestTTY(guestID(tty)); got != tty {
			t.Errorf("guestTTY(guestID(%q)) = %q", tty, got)
		}
	}
}
