# quack

Let someone into your terminal, or let your Claude or Codex talk to theirs,
peer-to-peer over [tailcat](https://github.com/tailscale/tailcat).

```
quack new                          your shell in a new session
quack claude / quack codex         an agent in a new session
Ctrl-Q  t a                        copy a terminal invite; you approve each guest
Ctrl-Q  c 1                        copy a one-use agent invite
! quack invite new agent           from any Claude or Codex: an agent invite, no session needed
```

The guest runs `quack join <link>` for a terminal, or pastes `! quack pair <link>`
into their agent.

## Install

```
curl -fsSL https://raw.githubusercontent.com/jreiml/quack/main/install.sh | sh
```

This installs a prebuilt macOS or Linux binary to `~/.local/bin` (override with
`QUACK_INSTALL_DIR`), and tmux 3.3 or newer if needed. You can also download a
binary from [releases](https://github.com/jreiml/quack/releases) or run
`go install github.com/jreiml/quack@latest`.

## Sessions

A session is a command running in its own tmux server. `quack new`,
`quack claude` and `quack codex` start one and attach your terminal, so run
them yourself. Without a terminal (for example, from an agent's shell tool)
they need `--detach`.

```
quack new -n build -- make watch         any command (default $SHELL)
quack claude -n auth-fix --model opus    Claude with --dangerously-skip-permissions
quack codex -n auth-fix                  Codex with --no-daemon, so quack can find its thread
```

`-n` names the session; Claude also uses it for its conversation. Other
arguments go to the agent, and `--` passes `-n` through. For Claude with its
permission prompts, use `quack new -- claude`.

`Ctrl-Q` opens the menu and `Ctrl-Q q` detaches. `quack ls`, `quack attach`,
`quack detach` and `quack stop` manage sessions. Without a name they use the
current session, then the only one, then ask.

## Invites

An invite is a link, `tc…/<id>`, of one of two kinds: a **terminal** invite
lets a person join your session, and an **agent** invite lets their agent
message yours. Each invite admits people in one of three ways:

| Admission | Who gets in |
|---|---|
| **Ask first** (default) | whoever you approve by their two-word code |
| **Allow next N** | the first N connections, then the invite is used up |
| **Allow anyone** | anyone with the link, until it's revoked or expires |

Create and manage invites from `Ctrl-Q` or the CLI:

```
quack invite new terminal                          ask first
quack invite new agent --auto-approve --limit 1    one use, no approval
quack invite new terminal --auto-approve --expires 2h
quack invite ls
quack invite set <id> --ask                        or --auto-approve [--limit N], --expires 30m
quack invite revoke <id> [--disconnect]
quack invite revoke --all                          revoke everything and disconnect everyone
quack invite copy <id>
quack allow <code> / quack decline <code>
```

`-n <session>` picks the session. `<id>` can be any unique prefix; the menu and
`invite ls` show six characters. The command is copied with `pbcopy`,
`wl-copy`, `xclip` or `xsel`, and always printed.

A revoked invite admits nobody new; existing connections stay unless you
disconnect them. Ask-first invites need you attached, so they stop when you
detach. Automatic invites keep working until they expire: after 24h from the
CLI unless you pass `--expires`, never from the menu unless you set one. Once
nothing usable is left and you're detached, sharing stops.

### Terminal guests

Guests can type and can only leave, not end the session. At most one Ctrl-C
every 3s reaches the session, and Ctrl-D doesn't. The window uses the host's
terminal size. Admitted guests reconnect with the same invite without asking
again.

| Event | Session | Sharing |
|---|---|---|
| guest leaves | keeps running | on |
| host detaches | keeps running | ask-first invites stop, automatic ones stay |
| host closes the terminal | as detach if automatic access or another host terminal remains, otherwise ends | as detach, or ends |
| command exits, or End session | ends | ends |

## Pairing agents

Two agents, Claude Code or Codex in any combination, can message each other.
Only the messages they send cross the link, not files or history.

1. The host creates an agent invite: `Ctrl-Q c` inside a `quack claude` or
   `quack codex` session, or `! quack invite new agent` typed into any Claude
   or Codex.
2. The guest pastes `! quack pair <link>` into their agent. If approval is
   needed, it prints a two-word code, keeps connecting in the background, and
   the host answers in `Ctrl-Q` or with `quack allow <code>`.
3. Both agents get a prompt that names the peer. Claude replies with
   `SendMessage`, Codex with `quack send <name> --message <text>`.

An agent hosting without a session shows up in `quack ls` as an agent host and
stops sharing when it exits. A session host must contain exactly one live
Claude or Codex.

`quack unpair [name]` ends pairings. They also end when the invite is revoked
with `--disconnect` or expires, an ask-first host detaches, either agent
exits, or the connection stays down for 10 minutes; the other agent is told.
Shorter outages go unnoticed: the guest reconnects and queued messages arrive
once, in order. Each direction allows 30 messages per 10 minutes, up to 32 KiB
each.

Agent names like `brave-otter-482731` stay stable across reconnects. Neither the
name nor the owner proves who someone is; check the approval code.

**Claude.** quack uses Claude Code's local messaging socket (checked against
2.1.281). A pairing belongs to the Claude process, not a conversation.
Messages are marked `from-mode="bypass"`: `quack claude` delivers them right
away, but a Claude in a prompting mode such as auto holds each one until you
approve it. To deliver them without asking, start that Claude with
`claude --settings '{"crossSessionInbound":"accept"}'`, or set **Messages from
your other sessions** to accept in `/config` for every session. Either accepts
messages from all your other sessions, not just quack.

**Codex.** quack needs `codex queue` (tested with 0.156.1) and a CLI session.
A pairing belongs to the thread, so switching threads ends it. Messages arrive
as queued input: an idle Codex starts a turn, a busy one reads them afterwards.
`quack codex` runs `codex --no-daemon`; a plain `codex` on the shared daemon can
pair too by running `quack invite` or `quack pair` itself. A brand-new Codex
gets its first message through its queue database, which needs `sqlite3`;
otherwise message it once before pairing.

Native messages are opt-in with `QUACK_CODEX_NATIVE=1`: `quack codex` then runs
Codex on its own app server, and a plain `codex` pairs natively when its
`quack invite` or `quack pair` sees the variable. They arrive the way Codex's own
`send_message_to_thread` delivers them, shown as "Sent by Codex from task <peer>
via quack": an idle Codex starts a turn at once, a busy one reads them in its
current turn. `quack codex` passes `-c`, `--enable` and `--disable` to the app
server too; `--profile` isn't supported.

In Codex's sandbox, `quack send` works but `quack pair`, `quack unpair` and
`quack invite` don't, so run them with escalated permissions or type them with
`!`.

### Plugin

The optional plugin gives agents two skills: `quack` (sharing, pairing and
working with a peer, including whether a peer message is genuine) and
`quack-setup` (installing quack).

```
/plugin marketplace add jreiml/quack             Claude Code
/plugin install quack@quack

codex plugin marketplace add jreiml/quack        Codex
codex plugin add quack@quack
```

## Security

- With ask-first invites, the link alone lets nobody in. Display names come
  from git config and aren't verified; the code is what you check.
- With automatic admission, anyone with the link gets in, up to its limit and
  expiry. Send it privately and set an expiry.
- A terminal guest can type, so they can run anything as you. `quack claude`
  skips Claude's permission prompts, so Claude won't ask before acting on what a
  guest types.
- A paired agent's messages reach an agent that may have broad permissions, for
  as long as the pairing lasts. Only pair with people you trust.
- Traffic is end-to-end encrypted with WireGuard, direct where NAT allows and
  otherwise through Tailscale's DERP relays, which only see encrypted packets.

## How it works

- Each session is its own tmux server (`tmux -L quack-<name>`) with Ctrl-Q as its
  only key binding. Its sharing state lives in tmux options.
- An agent host keeps its state in `$TMPDIR/quack/hosts/<name>/` and is tied to
  the agent's process.
- The first invite starts a tailcat server, `quack _serve`. The `tc…` address
  holds its key and a pre-shared key; each invite adds a random 128-bit ID.
- Each connection runs `quack _gate`, which derives the guest's code from their
  key and checks the invite. A terminal guest waits for approval and attaches.
  An agent's connection is relayed to `quack _pairhost`, one per pairing, which
  outlives dropped connections and takes the guest back by its tunnel key.
- Guests keep one key per link in `~/.config/quack/keys/`, deleted after 30 days
  unused. The host drops a guest after 45s without a keepalive.
- Tunnel keys exist only in memory, and no message history is stored. Logs are
  in `$TMPDIR/quack/`.

## Tests

```
go test ./...                     # unit and tmux tests
QUACK_NET_TEST=1 go test ./...    # also real tailcat round trips over DERP
```

## License

MIT
