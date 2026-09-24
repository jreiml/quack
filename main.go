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
  quack share [name]                  share and copy the join message
  quack allow <code>                  let a waiting guest in
  quack kick [who]                    disconnect a guest or turn away a waiting one
  quack unshare [name]                stop sharing; the link stops working
  quack stop [name]                   end the session
  quack join <link>                   join someone's session (Ctrl-Q q leaves)

Inside a session, Ctrl-Q opens the quack menu (share, allow, kick, unshare, detach).
`

var onFatal func(msg string)

func fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if onFatal != nil {
		onFatal(msg)
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
	case "kick":
		cmdKick(args)
	case "unshare":
		cmdUnshare(args)
	case "stop":
		cmdStop(args)
	case "join":
		cmdJoin(args)
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
