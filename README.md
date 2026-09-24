# quack

Let someone into your terminal. quack runs Claude (or any command) in a session you can share at any time, peer-to-peer over [tailcat](https://github.com/tailscale/tailcat). Guests get in only with a code you approve.

```
quack new           start claude --dangerously-skip-permissions in a hidden tmux session
Ctrl-Q  s           share: copies "quack join tc…" to paste to your guest
                    they run it and send you the code it shows, e.g. tiger-lamp
Ctrl-Q  1           "Let Ada Lovelace in (tiger-lamp)": check the code matches, they're in
```

## Letting people in without asking

A share has one setting, **new people**:

| | Who gets in | Ends |
|---|---|---|
| **ask first** (default) | whoever you let in | — |
| **next one joins** | the next person, without asking; then back to ask first | once used, or after 24h |
| **anyone joins** | everyone with the link, without asking | after 24h |

Pick it in the Ctrl-Q menu (`o` next one, `e` anyone, `m` back to ask first), or from a terminal:

```
quack share --auto-approve --limit 1           next one joins
quack share --auto-approve --expires 2h        anyone joins, for two hours
quack close                                    back to ask first
```

"Next one" and "anyone" are for handing a session over or being away: sharing stays on after you detach, until the end time. When they end, new people wait for your OK again, while the link and anyone already in stay. If the end time passes with nobody attached, sharing stops. With ask first, sharing stops as soon as you detach.

## The status bar

A one-line bar at the bottom always shows ` 🦆 Ctrl-Q`, so you know you're in quack and how to reach the menu. While sharing, it also shows what's going on:

```
 🦆 Ctrl-Q   🌐 Shared, ask first   👀 Ada Lovelace   ✋ Carl wants to join (code tiger-lamp)
 🦆 Ctrl-Q   🌐 Shared, next one joins until 16:30
 🦆 Ctrl-Q   🌐 Shared, anyone joins until 16:30   👀 Ada Lovelace, Bob
 🦆 Ctrl-Q   🌐 Shared, ask first, stays on until 16:30   👀 Ada Lovelace
```

Someone waiting is highlighted in yellow. After a menu action the bar shows what happened for a few seconds, e.g. `🦆 Ctrl-Q   🌐 Shared, ask first   🔗 Join link copied`. Guests see only the menu hint, whose session it is and the other guests: `🦆 Ctrl-Q   🏠 Johanna Reiml   👀 Bob`.

## The Ctrl-Q menu

Quack sessions show a 🦆 in front of the terminal title. Inside a session, `Ctrl-Q` opens a menu: share or copy the join link, let in a waiting guest (shown with their code) or turn them away, choose who new people get in, stop sharing, detach (`q`), end the session (`x`). tmux handles the key before the command sees it, so nothing reaches Claude's conversation. Ctrl-Q is the same key on German and English layouts, and Claude Code doesn't use it.

The same actions exist as commands (below) for use from another terminal.

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
| host detaches (Ctrl-Q q) | keeps running | stops, unless next one or anyone joins | "The host left, so sharing stopped." |
| host closes the terminal (e.g. Ctrl-W in kitty) | ends, unless next one or anyone joins or another of your terminals is attached; then as detach | ends | "The host ended the session." |
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
| `quack share [name]` | share and copy the join command; run it again to re-copy |
| `quack share --auto-approve [--limit N] [--expires 2h]` | share and let people in without asking (anyone, or the next N) |
| `quack close [name]` | back to ask first |
| `quack allow <code>` | let a waiting guest in |
| `quack decline <code>` | turn away a waiting guest |
| `quack unshare [name]` | stop sharing; the link stops working and the session keeps running |
| `quack stop [name]` | end the session |
| `quack join <link>` | guest side; Ctrl-Q opens the guest menu, `q` leaves |

When there is no name, commands use the session you're in (`$QUACK_SESSION`), then the only session running, then a picker.

## How it works

- Each session is its own tmux server (`tmux -L quack-<name>`) with no prefix key and no key bindings other than Ctrl-Q, so it feels like running the command directly. The status bar shows the host and guests different text.
- `share` starts a tailcat server inside that tmux server, as a hidden `_serve` session. The link (a `tc…` address) contains the server's key and a pre-shared key, and it changes on every share.
- Every SSH connection runs `quack _gate`. It derives a two-word code from the guest's tunnel key, which `quack join` also shows on the guest's side. It waits until you let that code in (from the menu, or `quack allow <code>`), then attaches to the session.
- Guests keep one key per link in `~/.config/quack/keys/`, so reconnecting to the same link doesn't need a new code until the share ends. Several joins to the same link from one machine each get their own key, and keys unused for 30 days are deleted.
- Logs: `$TMPDIR/quack/<name>.log`.

## Security

- In a normal share, the link alone doesn't get anyone in: every new guest needs your `allow`.
- With next one or anyone joins, the link is the key: whoever has it gets in, up to the limit and until it expires. Send it in a direct message, not a channel. Next one joins is the safe choice for handing over to one person: if someone else used the link first, your person ends up waiting and will tell you.
- To get someone out, stop sharing: everyone is disconnected and the link stops working. Share again for a new link and send it only to the people who should stay.
- An allowed guest can type, which means they can run anything as you, and by default Claude runs with `--dangerously-skip-permissions`, so it won't ask before acting on what they type. Only allow people you're talking to right now. Use `quack new -- claude` for a session that asks.
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
