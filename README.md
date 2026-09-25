# quack

Let someone into your terminal, or let your Claude or Codex talk to theirs.
quack runs a command in a tmux session that you can share at any time,
peer-to-peer over [tailcat](https://github.com/tailscale/tailcat).

```
quack new           your shell in a new session
quack claude        Claude in a new session
quack codex         Codex in a new session
Ctrl-Q  t a         copy a terminal invite; you approve each guest
Ctrl-Q  c 1         copy a one-use agent invite
Ctrl-Q  m           manage invites and connections
```

## Install

```
curl -fsSL https://raw.githubusercontent.com/jreiml/quack/main/install.sh | sh
```

This installs a prebuilt macOS or Linux binary to `~/.local/bin` (override with
`QUACK_INSTALL_DIR`), and tmux if needed. quack needs tmux 3.3 or newer. You can
also download a binary from [releases](https://github.com/jreiml/quack/releases)
or run `go install github.com/jreiml/quack@latest`.

## Sessions

`quack new`, `quack claude` and `quack codex` start a session in its own tmux
server and attach your terminal to it. Run them yourself in a terminal. Without
a terminal (for example, from an agent's shell tool) they refuse to run unless
you pass `--detach`. They also refuse inside a quack session.

```
quack new -n build -- make watch         any command (default $SHELL)
quack claude -n auth-fix --model opus    Claude with --dangerously-skip-permissions
quack codex --name auth-fix              Codex with --no-daemon, so quack can find its thread
quack new --detach -- sleep 600          start in the background and print the name
```

`-n`/`--name` names the session. Claude also gets this name for its conversation;
Codex doesn't (use `/rename` there). Other arguments go to the agent. Use `--`
to pass `-n` or `--name` to the agent itself. For Claude without
`--dangerously-skip-permissions`, use `quack new -- claude`.

Ctrl-Q opens the menu. `Ctrl-Q q` detaches and leaves the session running.
Reattach with `quack attach`, list sessions with `quack ls` and end one with
`quack stop`. Session commands without a name use the current session, then the
only one, then ask.

## Sharing

An invite is a link of the form `tc…/<invite-id>`. There are two types:
terminal invites (`quack join <link>`) and agent invites (`! quack pair <link>`).
Terminal invites can't pair agents, and agent invites can't attach terminals.
Copying an invite puts that command on your clipboard. quack uses `pbcopy`,
`wl-copy`, `xclip` or `xsel`. Over SSH, it falls back to OSC 52 and also shows
the command so you can copy it yourself.

| Admission | Who gets in |
|---|---|
| **Ask first** (default) | whoever you approve by their two-word code |
| **Allow next N** | the first N connections; then the invite is used up |
| **Allow anyone** | anyone with the link, until it is revoked or expires |

Create invites from the menu or the CLI:

```
quack share                                    terminal invite, ask first
quack share --pair                             agent invite, ask first
quack share --pair --auto-approve --limit 1    one-use agent invite
quack share --auto-approve --expires 2h        open terminal invite for two hours
quack allow <code> / quack decline <code>      answer a waiting guest
quack close                                    make open invites ask first again
quack unshare                                  revoke everything and disconnect everyone
```

In the menu, **Manage access** lists every invite. You can copy an invite again,
change its admission or expiry, disconnect individual people, revoke it, or
revoke it and disconnect everyone who used it. You can also stop all terminal
access or all agent pairing at once. A revoked invite admits nobody new, but
existing connections stay up. Disconnecting never kills the session.

Ask-first invites need you to be attached, so they stop when you detach.
Automatic invites keep working after you detach until they expire. Menu invites
never expire unless you set an expiry. CLI automatic invites expire after 24h.
When nothing usable is left and you're detached, the share server stops.
Admitted terminal guests can reconnect with the same invite and saved key.
Invites live in the tmux session and disappear with it.

The status bar always shows ` 🦆 Ctrl-Q`, plus open invites, guests, pairs and
waiting requests (in yellow, with their code).

### Keys and endings

Guests can only leave. At most one Ctrl-C every 3s from a guest reaches the
session, so a double tap can't quit Claude, and guests can't send Ctrl-D.
The shared window uses the host's terminal size.

| Event | Session | Sharing |
|---|---|---|
| guest leaves | keeps running | on |
| host detaches (`Ctrl-Q q`) | keeps running | ask-first invites stop; automatic access stays |
| host closes the terminal | ends, unless automatic access or another host terminal remains; then as detach | ends, or as detach |
| command exits, or End session | ends | ends |

## Pairing agents

Paired Claude Code and Codex CLI sessions can message each other in any
combination. Only messages they send each other cross the link. Conversation
history and files are not shared.

1. The host copies an agent invite (`Ctrl-Q c a`, or `c 1` for no approval).
2. The guest pastes `! quack pair tc…/<invite-id>` into their agent's prompt.
   The agent can also run the command through its shell tool.
3. If approval is required, the host sees
   **Let brave-otter-482731 (Ada Lovelace) in** in Ctrl-Q, with a code to check.
4. Both agents get a prompt that introduces the peer. Claude replies with its
   native `SendMessage` to the inbox name in that prompt. Codex replies with
   `quack send <name> --message <text>`.

`quack pair` waits up to three seconds. If the pairing isn't ready by then, it
prints the approval code and keeps connecting in the background. The agent is
told once the host lets it in or the connection fails. Idle agents wait for
their human instead of starting a conversation.

Each agent session has a stable name such as `brave-otter-482731`, taken from
the quack session name or generated. The six digits come from the session and
process identity. The name stays the same across reconnects and stays unique
per live agent session. Neither the name nor the owner label proves who
someone is. Check the approval code.

`quack unpair [name]` ends the agent's pairings, or only the one you name by
peer, owner or inbox. Pairings also end when the host disconnects them or stops
agent access, the invite expires, an ask-first host detaches, or either agent
exits. The
other agent is told when a pairing ends. Each direction allows 30 messages per
10 minutes, each up to 32 KiB of text.

**Claude.** This uses Claude Code's internal local messaging protocol, checked
against 2.1.281 on macOS and Linux. The host window must contain exactly one live
Claude or Codex. Messages are marked `from-mode="bypass"`, as for quack's
default launch. Claude can still hold or refuse them in a prompting permission
mode or under an inbound message policy.

**Codex.** This requires `codex queue` (tested with 0.156.1) and a local CLI
session that owns its thread. Shared-daemon, App Server and desktop sessions
don't work. Peer messages arrive as queued user input: an idle Codex starts a
turn, and a busy one reads them after its current turn. A new Codex has no
saved thread until its first message. quack delivers that first message through
Codex's queue database with `sqlite3`. Without `sqlite3`, send Codex a message
before pairing. `quack send` works inside Codex's sandbox. `quack pair` and
`quack unpair` don't, so run them with escalated permissions or type them with
`!`. Pair records live in `$CODEX_HOME/quack-pairs`.

Tunnel keys exist only in memory. On a normal shutdown, quack removes sockets,
keys, inbox records and the `$TMPDIR/quack/pair-<pid>.log` log. None of these
contain message history.

## Security

- With ask-first invites, the link alone doesn't let anyone in. You approve
  each person by code. Display names come from git config and aren't verified.
- With automatic admission, anyone with the link gets in, up to the invite's
  limit and expiry. Send it privately and set an expiry.
- A terminal guest can type, so they can run anything as you. `quack claude`
  skips Claude's permission prompts, so Claude won't ask before acting on what a
  guest types.
- A paired agent's messages reach an agent that may have broad permissions.
  Approving a pairing grants ongoing message access, and prompt-injection
  checks don't make it safe. Only pair with people you trust.
- Traffic is end-to-end encrypted with WireGuard. It goes direct where NAT
  allows, and otherwise through Tailscale's DERP relays, which only see
  encrypted packets.

## How it works

- Each session is its own tmux server (`tmux -L quack-<name>`) with Ctrl-Q as
  its only key binding.
- `share` starts a tailcat server inside that tmux server. The `tc…` address
  holds the server key and a pre-shared key. Each invite adds a random 128-bit
  ID.
- Each SSH connection runs `quack _gate`. The gate derives the guest's code from
  their key and checks the invite's type, expiry and allowance. It waits for
  approval if needed, then attaches the guest's terminal or starts the message
  bridge.
- Guests keep one key per link in `~/.config/quack/keys/`, which is deleted
  after 30 days unused. The host drops a guest after 45s without a keepalive.
- Logs are in `$TMPDIR/quack/<name>.log`.

## Tests

```
go test ./...                     # unit and tmux tests
QUACK_NET_TEST=1 go test ./...    # also a real tailcat round trip over DERP
```

## License

MIT
