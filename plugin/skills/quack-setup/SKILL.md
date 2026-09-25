---
name: quack-setup
description: Install quack when it's missing. Use only when `quack` isn't found on PATH, or when it fails because tmux or another dependency is missing or too old. For using an installed quack, see the quack skill.
---

# Installing quack

- quack needs tmux 3.3 or newer (`tmux -V`).
- The install script puts a prebuilt macOS/Linux binary in `~/.local/bin` and
  installs tmux if needed. It changes the user's machine, so ask before running
  it:

  ```
  curl -fsSL https://raw.githubusercontent.com/jreiml/quack/main/install.sh | sh
  ```

  Alternatives: a binary from https://github.com/jreiml/quack/releases, or
  `go install github.com/jreiml/quack@latest`.
- If `~/.local/bin` isn't on PATH, the new binary won't be found until it is.
- Optional:
  - `pbcopy`, `wl-copy`, `xclip` or `xsel` for copying invites. Over SSH quack
    falls back to OSC 52 and prints the command.
  - `sqlite3`, which lets quack deliver the first message to a brand-new Codex
    session.
  - A Claude in a prompting permission mode such as auto holds each peer
    message for approval; `quack claude` doesn't. To deliver them without
    asking, the human can start Claude with
    `claude --settings '{"crossSessionInbound":"accept"}'`, or set **Messages
    from your other sessions** to accept in `/config` for every session. It
    accepts messages from all their other sessions, so it's their call; don't
    change it for them.

Once it's installed, `quack --help` lists the commands.
