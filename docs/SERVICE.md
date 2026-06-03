# Optional service units

ClipBeam runs **daemonless by default** (the SSH-exec push path needs no running
process and no open port). A long-lived receiver is opt-in and **never installed
by `curl|sh`, Homebrew, or `clipbeam setup`** (PLAN §9.7).

Manage the unit with the CLI; it writes the platform-native unit file for you:

```sh
clipbeam service install     # install + load the unit (off by default)
clipbeam service uninstall   # unload + remove it
```

## Linux — `systemd --user`

`clipbeam service install` writes `~/.config/systemd/user/clipbeam.service`,
running `clipbeam serve` with `Restart=on-failure` and a loopback / unix-socket
bind. It is a **user** unit (no root), enabled per-user:

```ini
[Unit]
Description=ClipBeam receiver
After=network.target

[Service]
ExecStart=%h/.local/bin/clipbeam serve
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
```

Enable lingering (`loginctl enable-linger "$USER"`) if you want it to run while
you are logged out.

## macOS — `launchd` LaunchAgent

`clipbeam service install` writes a LaunchAgent under `~/Library/LaunchAgents`
running `clipbeam serve`. The label and the Keychain service both use the frozen
identifier **`com.sani.clipbeam`** (PLAN §9.7, §10.2) — note this is the brand
identifier, not the binary name. The agent binds a non-8787 port or a unix
socket with the token required (PLAN §6.3); if `ClipBeam.app` is already running,
the daemon detects port-in-use on bind and exits with a clear diagnostic rather
than fighting the app for `~/.clipbeam` state (PLAN §7.6).

## When you do NOT need a service

- **Push-only over SSH** (`clipbeam send … user@host`): daemonless-exec, no unit.
- **Tailscale always-on receiver**: a unit is convenient but optional; you can
  also run `clipbeam serve --listen tailscale` in any supervisor you already use.
