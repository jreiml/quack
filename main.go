package main

import (
	"fmt"
	"os"
)

const usage = `quack: share a terminal or pair Claude/Codex agents, peer-to-peer over tailcat

Sessions. These take over the terminal, so run them yourself:
  quack new [-n name] [-- cmd]        run cmd (default $SHELL) in a new session
  quack claude [-n name] [args...]    Claude with --dangerously-skip-permissions
  quack codex [-n name] [args...]     Codex with --no-daemon
      --detach                        start in the background and print the name
  quack ls                            list sessions and agent hosts
  quack attach|detach|stop [name]     reattach, detach your terminals, or end a session

Invites. -n picks the session; otherwise the current one, the only one, or ask:
  quack invite new terminal|agent     create and copy an invite; you approve each guest
      --auto-approve [--limit N]      admit without asking, for 24h unless --expires
      --expires 2h|never              revoke and disconnect after this long
  quack invite ls                     list invites
  quack invite set <id> <options>     --ask, --auto-approve [--limit N], --expires
  quack invite revoke <id>            stop admitting; --disconnect also drops its guests
  quack invite revoke --all           revoke everything and disconnect everyone
  quack invite copy <id>              copy an invite again
  quack allow|decline <code>          answer someone waiting to get in
Run "! quack invite new agent" inside Claude or Codex to host without a session.

Joining:
  quack join <link>                   join someone's terminal
  quack pair <link>                   pair this agent with theirs; run it inside the agent
  quack send <name> --message <text>  Codex: message a paired agent (Claude: SendMessage)
  quack unpair [name]                 end this agent's pairings

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
	case "invite":
		cmdInvite(args)
	case "allow":
		cmdAllow(args)
	case "decline":
		cmdDecline(args)
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
