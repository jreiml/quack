# quack

Let someone into your terminal. quack runs Claude (or any command) in a session you can share at any time, peer-to-peer over [tailcat](https://github.com/tailscale/tailcat). Invites control who can join your terminal or pair a Claude session.

```
quack new           start claude --dangerously-skip-permissions in a hidden tmux session
Ctrl-Q  t a         copy a terminal invite that asks before admitting
Ctrl-Q  c 1         copy a one-use Claude invite, admitted without asking
Ctrl-Q  m           manage invites and their connections
```

## Invites and access

Every invite has its own link, type (terminal or Claude), admission policy,
expiry and associated connections. Both types use the same tailcat server,
but a terminal invite cannot pair an agent and a Claude invite cannot attach a
terminal. The clipboard contains exactly one command:

```
quack join tc…/<invite-id>
! quack pair tc…/<invite-id>
```

The Ctrl-Q menu has four main views:

1. **Main:** invite to terminal (`t`), invite a Claude (`c`), manage access (`m`),
   stop all access (`s`), detach (`q`) and end session (`x`). Pending approval
   requests appear at the top.
2. **Create invite:** copy with approval required (`a`), allow one connection
   (`1`), or allow anyone (`e`). Set expiry (`x`) before copying if needed.
3. **Manage access:** select an invite, stop all agent messaging (`c`), or stop
   all terminal access (`t`). Entries show an invite ID, policy, connections
   and expiry.
4. **Invite details:** copy its command, change admission or expiry, disconnect
   individuals, revoke the invite, or revoke and disconnect everyone using it.

Admission and expiry have small choice/input prompts. Bulk disconnections ask
for confirmation. Escape goes back. Guests only get a Leave action.

| Admission | Who gets in | After the allowance is used |
|---|---|---|
| **Ask before admitting** | whoever you approve by code | keeps asking |
| **Allow one / next N** | the first N connections holding this invite | consumed; new connections are rejected |
| **Allow anyone** | everyone holding this invite | until revoked or expired |

An admitted terminal guest can reconnect using the same invite and saved key
without consuming another admission. Each Claude pairing is a new connection.
Consumed invites remain manageable while they have connections or terminal
reconnect permissions. Explicitly changing their admission policy can reopen
them; `quack close` leaves them consumed. Revoked or expired invites cannot be
reopened. Dead entries are removed after their connections finish.

**Revoke invite** blocks future connections, including reconnects, and cancels
pending requests; existing connections continue. **Revoke and disconnect all**
also ends its terminal attachments or agent pairings. **Stop all agent messaging**
revokes every Claude invite and disconnects pairs, leaving terminal access
running—useful for a handover. Disconnecting does not kill either underlying
Claude session. An individual disconnect removes its admission; an open invite
can still be used to request admission again.

Expiry revokes the invite and disconnects its connections. Once no usable
invites or connections remain and the host is detached, the share server stops. Ask-first invites
require an attached host and end when the last host detaches. From a detached
session, attach before using `quack share` without automatic admission or
`quack close`; use `--auto-approve` for an unattended handover. Automatically admitted access can remain
after detach, including connections through consumed or revoked invites, until
disconnected or expired. The menu defaults to no expiry; CLI automatic invites
default to 24 hours. Explicitly choose an expiry for unattended access.

From a terminal:

```
quack share                                  create a terminal invite, ask first
quack share --pair                           create a Claude invite, ask first
quack share --pair --auto-approve --limit 1    create a one-use Claude invite
quack share --auto-approve --expires 2h       allow terminal connections for two hours
quack close                                  change open invites back to ask first
```

Each `quack share` creates a new invite. Re-copy an existing command in Manage
access. `quack unshare` revokes everything, disconnects everyone and stops the
underlying server. Invites live in tmux options and disappear with the session.

## Pairing agents

Two Claude Code sessions can message each other through a Claude invite.
Use Ctrl-Q → c → a to copy an invite requiring host approval, or c → 1 to
allow one pairing without asking. Ada Lovelace pastes the copied
`! quack pair tc…/<invite-id>` into Claude's shell mode. This authorizes receiving
messages on Ada's side. When approval is required, the host sees
**Let brave-otter-482731 (Ada Lovelace) in (tiger-lamp)** in Ctrl-Q, with a reminder
that approving lets that peer's messages reach Claude without asking again.
Check the code before allowing. `quack allow` and `quack decline` work too.
Terminal invites and their allowances are independent of Claude invites.

The command waits up to three seconds for a connection and approval. If ready,
it prints a prompt explaining who Claude is paired with and how to send a
message. Otherwise it prints the code and returns; a background process tells
Claude when approval happens or connecting fails. No skill or plugin is needed.
The host's Claude also gets a pairing prompt. Both use native `SendMessage` to
the inbox name in that prompt; only those messages cross the link.
Conversation history and files are not shared.

Each live Claude session has a name such as `brave-otter-482731`, displayed as
`brave-otter-482731 (Ada Lovelace)`. It reuses the quack session name when there
is one; standalone Claudes get an adjective-animal name. Six digits derived
from a hash of the inbox's random token and process identity reduce collisions
without revealing that token. The name stays the same across pairings and
reconnections to that live session, including when several peers connect to
it. Restarting Claude gives it a new identity. The approval code remains
separate, and neither a session name nor an owner label verifies a person.

Pairing notices and incoming messages use the same display name. The prompt
asks Claude to use `SendMessage` with the inbox name, also visible in
`ListAgents`. If that name is already in use locally (for example, two Claudes
on one machine pairing with the same remote session), the local inbox alias
gets another six-digit suffix. The remote session's identity and display name
stay the same. Always use the exact inbox name from your pairing prompt.

The host's bar shows `🤖 brave-otter-482731 (Ada Lovelace)` and names waiting
sessions alongside their approval code. `quack ls` counts active pairs.

Run `quack unpair` inside Claude to end its pairings, or
`quack unpair brave-otter-482731` to select a session. You can also select an
owner with `quack unpair "Ada Lovelace"`, or an exact local inbox alias.
Stopping agent access, stopping all sharing, invite expiry, host detach for an ask-first invite, or either Claude exiting ends the pairing. The surviving
Claude gets an ending notice. Each direction allows 30 messages per rolling
10 minutes, with a notice to the sender when the cap drops a message. Messages
are limited to 32 KiB of text; attachments and delivery receipts are not bridged.

Both peers need this version of quack for invite links (named pair protocol 2).
Old links containing only a `tc…` address are no longer accepted; create a new
invite to get a link with an invite ID.

Pairing uses Claude Code's internal local messaging protocol, inspected in
2.1.281 on macOS and Linux, and requires a live messaging socket and key file. The host
must have exactly one Claude inbox under the shared tmux window; the guest is
identified from the calling Claude process. The bridge declares
`from-mode="bypass"`, matching quack's default Claude launch. This does not change
Claude's tool permissions or settings. A prompting-mode Claude, or an explicit
inbound hold/refuse policy, may still hold or reject messages. Socket delivery
is not proof of model acceptance. Current permission modes are not reliably
observable, so the menu warns about this limitation instead of claiming to
detect mode differences.

Pair tunnel keys exist only in memory. Normal shutdown removes the bridge's
socket, local authentication key, Claude registry entry, inbox-name claim
and temporary `$TMPDIR/quack/pair-<pid>.log` diagnostic log. A forced kill or
machine crash cannot run cleanup; these files contain no message history.

## The status bar

A one-line bar at the bottom always shows ` 🦆 Ctrl-Q`, so you know you're in quack and how to reach the menu. While sharing, it also shows what's going on:

```
 🦆 Ctrl-Q   🌐 2 invites   👀 Ada Lovelace
 🦆 Ctrl-Q   🌐 2 invites   🤖 brave-otter-482731 (Ada Lovelace)
```

Waiting requests are highlighted in yellow with their approval code. After a
menu action the bar briefly shows its result. Guests see the menu hint, host
name and other terminal guests. Use Manage access for each invite's policy,
remaining admissions and expiry. The bar counts only invites accepting new
connections; connected guests and pairs appear separately.

## Keys and endings

You own the session; guests can only leave.

| | Host | Guest |
|---|---|---|
| Esc / Ctrl-C | as usual | as usual, but at most one Ctrl-C every 3s reaches the session, so a double tap can't quit Claude |
| Ctrl-D | as usual | ignored |
| Ctrl-Q | menu | menu with Leave |
| Ctrl-Q q | detach, the session keeps running | leave |
| Ctrl-C while waiting to be let in | — | cancels |

| What happens | Session | Sharing | Guest sees |
|---|---|---|---|
| guest leaves or closes their terminal | keeps running | on | — |
| host detaches (Ctrl-Q q) | keeps running | ask-first invites stop; automatic access can remain | "The host left, so sharing stopped." |
| host closes the terminal (e.g. Ctrl-W in kitty) | ends, unless automatic access remains or another host terminal is attached; then as detach | ends | "The host ended the session." |
| host quits the command, or End session | ends | ends | "The host ended the session." |
| Stop sharing | keeps running | stops | "The host stopped sharing." |
| Turn away (someone waiting) | keeps running | on | "The host declined." They can ask again. |

The shared window always has the host's terminal size. Guests with a bigger terminal see blank space around it; with a smaller one they see the top-left part.

## Install

```
curl -fsSL https://raw.githubusercontent.com/jreiml/quack/main/install.sh | sh
```

This puts a prebuilt binary for macOS or Linux in `~/.local/bin` (set `QUACK_INSTALL_DIR` to change it). Binaries are also on the [releases page](https://github.com/jreiml/quack/releases). With Go installed, `go install github.com/jreiml/quack@latest` works too.

It needs tmux 3.3 or newer (`brew install tmux`, `apt install tmux`). A handy alias: `alias cc='quack new --'`.

## Commands

| | |
|---|---|
| `quack new [-n name] [-s] [-- cmd]` | start a session (default `claude --dangerously-skip-permissions`) and attach; `-s` shares it right away |
| `quack ls` | sessions with dir, age, attached, shared, guests, waiting |
| `quack attach [name]` / `quack detach` | reattach / detach; the session keeps running |
| `quack share [--pair] [name]` | create and copy a new terminal or Claude invite |
| `quack share --auto-approve [--limit N] [--expires 2h]` | create an automatic invite (anyone, or the next N); add `--pair` for Claude |
| `quack close [name]` | change open invites back to ask first; consumed invites stay closed |
| `quack allow <code>` | let a waiting guest in |
| `quack decline <code>` | turn away a waiting guest |
| `quack unshare [name]` | stop sharing; the link stops working and the session keeps running |
| `quack stop [name]` | end the session |
| `quack pair <link>` | pair your Claude; run as `! quack pair tc…/<invite-id>` inside Claude |
| `quack unpair [name]` | end your Claude's pairings, or select a peer/inbox by name |
| `quack join <link>` | guest side; Ctrl-Q opens the guest menu, `q` leaves |

For session commands without a name, quack uses the session you're in (`$QUACK_SESSION`), then the only session running, then a picker.

## How it works

- Each session is its own tmux server (`tmux -L quack-<name>`) with no prefix key and no key bindings other than Ctrl-Q, so it feels like running the command directly. The status bar shows the host and guests different text.
- `share` starts a tailcat server inside that tmux server, as a hidden `_serve` session. The `tc…` address contains the server's key and a pre-shared key; each invite adds an independent random 128-bit ID. Stopping the server invalidates all its links.
- Every SSH connection runs `quack _gate`. It derives a two-word code from the guest's tunnel key, which `quack join` also shows on the guest's side. It checks the invite type, revocation, expiry and admission allowance, then waits for approval if required. An admitted terminal guest attaches; an admitted agent gets the message bridge.
- Guests keep one key per link in `~/.config/quack/keys/`, so admitted guests can reconnect until the invite is revoked or expires. Several joins to the same link from one machine each get their own key, and keys unused for 30 days are deleted.
- Logs: `$TMPDIR/quack/<name>.log`.

## Security

- In a normal share, the link alone doesn't get anyone in: every new guest needs your `allow`.
- With automatic admission, the invite is the key: whoever holds it gets in up to its allowance and expiry. Send it privately. A consumed invite rejects new connections; create another if the intended recipient did not get in.
- Revoke an invite to block future connections; revoke and disconnect to also end its existing connections. Stop agent or terminal access independently in Manage access.
- An allowed guest can type, which means they can run anything as you, and by default Claude runs with `--dangerously-skip-permissions`, so it won't ask before acting on what they type. Only allow people you're talking to right now. Use `quack new -- claude` for a session that asks.
- A paired agent's messages reach a Claude that may run with `--dangerously-skip-permissions` and act as you. Pairing approval grants ongoing message access; prompt-injection checks are not an authorization boundary. Only pair with people you trust. Automatic Claude invites admit agents holding that invite.
- Traffic is end-to-end encrypted (WireGuard). It goes peer-to-peer where NAT allows, otherwise through Tailscale's public DERP relays, which see only encrypted packets.
- The guest's display name comes from their git config and isn't verified. The code is what identifies them.

Guests need `quack` too: `quack join` gives them a terminal and sends keepalives. The host drops a guest after 45 seconds of silence.

## Tests

```
go test ./...                     # unit + tmux tests
QUACK_NET_TEST=1 go test ./...    # also a real tailcat round trip over DERP
```

## License

MIT
