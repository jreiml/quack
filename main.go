package main

import (
	"fmt"
	"os"
)

const usage = `quack: share a terminal or pair Claude/Codex agents, peer-to-peer over tailcat

Start a session. These open a tmux session and take over this terminal, so run
them yourself. An agent should not run them from its shell tool:
  quack new [-n name] [-s] [-- cmd]   run cmd (default $SHELL) in a new session
  quack claude [-n name] [args...]    run Claude with --dangerously-skip-permissions
  quack codex [-n name] [args...]     run Codex with --no-daemon
      --detach                        start in the background and print the name
  Other claude/codex arguments go to the agent; -- ends quack's options.

Sessions:
  quack ls                            list sessions
  quack attach [name]                 reattach this terminal
  quack detach [name]                 detach your terminals; the session keeps running
  quack stop [name]                   end the session

Sharing:
  quack share [name]                  create and copy a terminal invite; you approve each guest
      --pair                          create an agent invite instead
      --auto-approve                  admit without asking (default expiry 24h)
      --limit N                       with --auto-approve: only the next N connections
      --expires 2h|never              revoke and disconnect after this long
  quack allow <code>                  let a waiting guest in
  quack decline <code>                turn away a waiting guest
  quack close [name]                  make open invites ask first again
  quack unshare [name]                revoke all invites and disconnect everyone
  quack join <link>                   join someone's terminal

Agent pairing. Run by the agent through its shell tool, or typed with "!" in its prompt:
  quack pair <link>                   pair this agent with the host's agent
  quack send <name> --message <text>  Codex only: message a paired agent (Claude uses SendMessage)
  quack unpair [name]                 end this agent's pairings

Without a name, session commands use the current session, then the only one, then ask.
Inside a session, Ctrl-Q opens the menu.
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
	case "claude", "codex":
		cmdAgent(cmd, args)
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
	case "send":
		cmdSend(args)
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
	case "_invite-command":
		cmdInviteCommand(args)
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
