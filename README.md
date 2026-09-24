# quack

Let someone into your terminal. quack runs Claude (or any command) in a session you can share at any time, peer-to-peer over [tailcat](https://github.com/tailscale/tailcat). Guests get in only with a code you approve.

```
quack new           start claude in a hidden tmux session
Ctrl-Q  s           share: copies "quack join tc…" to paste to your guest
                    they run it and send you the code it shows, e.g. tiger-lamp
Ctrl-Q  1           "Let in Ada Lovelace · tiger-lamp": check the code matches, they're in
```

## The Ctrl-Q menu

Inside a session, `Ctrl-Q` opens a menu: share or copy the join command again, let in a waiting guest (shown with their code), turn one away, kick a guest, stop sharing, detach (`q`), end the session (`x`). tmux handles the key before the command sees it, so nothing reaches Claude's conversation. Ctrl-Q is the same key on German and English layouts, and Claude Code doesn't use it.

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
| host detaches or closes the terminal | keeps running | stops | "The host left, so sharing stopped." |
| host quits the command, or End session | ends | ends | "The host ended the session." |
| Stop sharing | keeps running | stops | "The host stopped sharing." |
| Kick | keeps running | on | "The host removed you." |

The shared window always has the host's terminal size. Guests with a bigger terminal see blank space around it; with a smaller one they see the top-left part.

## Install

```
go install github.com/jreiml/quack@latest
alias cc='quack new --'
```

It needs tmux 3.3 or newer. Go 1.27.1 is fetched automatically on the first build (`GOTOOLCHAIN=auto`).

## Commands

| | |
|---|---|
| `quack new [-n name] [-s] [-- cmd]` | start a session (default `claude`) and attach; `-s` shares it right away |
| `quack ls` | sessions with dir, age, attached, shared, guests, waiting |
| `quack attach [name]` / `quack detach` | reattach / detach; the session keeps running |
| `quack share [name]` | share and copy the join message; run it again to re-copy |
| `quack allow <code>` | let a waiting guest in |
| `quack kick [who]` | disconnect a guest (or turn away a waiting one); they need a new approval |
| `quack unshare [name]` | stop sharing; the link stops working and the session keeps running |
| `quack stop [name]` | end the session |
| `quack join <link>` | guest side; Ctrl-Q opens the guest menu, `q` leaves |

When there is no name, commands use the session you're in (`$QUACK_SESSION`), then the only session running, then a picker.

## How it works

- Each session is its own tmux server (`tmux -L quack-<name>`) with no prefix key, no key bindings and no status bar, so it feels like running the command directly. The status bar appears only while someone is waiting or watching.
- `share` starts a tailcat server inside that tmux server, as a hidden `_serve` session. The link (a `tc…` address) contains the server's key and a pre-shared key, and it changes on every share.
- Every SSH connection runs `quack _gate`. It derives a two-word code from the guest's tunnel key, which `quack join` also shows on the guest's side. It waits until you let that code in (from the menu, or `quack allow <code>`), then attaches to the session.
- Guests keep their key in `~/.config/quack/client.key`, so reconnecting doesn't need a new code until they're kicked or the share ends.
- Logs: `$TMPDIR/quack/<name>.log`.

## Security

- The link alone doesn't get anyone in: every new guest needs your `allow`.
- An allowed guest can type, which means they can run anything as you. Only allow people you're talking to right now.
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
