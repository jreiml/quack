package main

import (
	"fmt"
	"os"
)

const usage = `quack: shareable terminal sessions over tailcat

  quack new [-n name] [-s] [-- cmd]   start a session (default: claude --dangerously-skip-permissions) and attach; -s shares it at once
  quack ls                            list sessions
  quack attach [name]                 reattach (default: most recent)
  quack detach [name]                 detach your terminals, keep the session running
  quack share [name]                  create and copy a new terminal invite; approval required
      --pair                          create a Claude invite instead
      --auto-approve                  let people in without asking (runs on after you detach)
      --limit N                       only the first N connections, then reject new ones
      --expires 2h                    revoke and disconnect after this long; use never for no expiry
                                      automatic invites default to 24h
  quack close [name]                  change open invites back to asking first; used invites stay closed
  quack allow <code>                  let a waiting guest in
  quack decline <code>                turn away a waiting guest
  quack unshare [name]                stop sharing; everyone is disconnected and the link stops working
  quack stop [name]                   end the session
  quack pair <link>                   pair Claude agents (run inside Claude with !)
  quack unpair [name]                 end an agent pairing
  quack join <link>                   join someone's session (Ctrl-Q q leaves)

Inside a session, Ctrl-Q opens the quack menu (invite to terminal, invite a Claude, manage access, detach).
`

var onFatal func(msg string)

func fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if f := onFatal; f != nil {
		onFatal = nil
		f(msg)
	}
	fmt.Fprintln(os.Stderr, "quack: "+msg)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "new":
		cmdNew(args)
	case "ls", "list":
		cmdLs(args)
	case "attach", "a":
		cmdAttach(args)
	case "detach":
		cmdDetach(args)
	case "share":
		cmdShare(args)
	case "allow":
		cmdAllow(args)
	case "decline":
		cmdDecline(args)
	case "close":
		cmdClose(args)
	case "unshare":
		cmdUnshare(args)
	case "stop":
		cmdStop(args)
	case "join":
		cmdJoin(args)
	case "pair":
		cmdPair(args)
	case "unpair":
		cmdUnpair(args)
	case "_pair":
		cmdPairWorker(args)
	case "_expire":
		cmdExpire(args)
	case "_serve":
		cmdServe(args)
	case "_gate":
		cmdGate(args)
	case "_menu":
		cmdMenu(args)
	case "_act":
		cmdAct(args)
	case "_fit":
		cmdFit(args)
	case "_quit":
		cmdQuit(args)
	case "_detached":
		cmdDetached(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "quack: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}
