# Platform setup

The Go bridge and Python MCP server are designed to run on macOS, Linux, and Windows. The macOS Contacts.app helper is optional and does not affect core WhatsApp contact or chat search. Live account testing for this fork has been performed on macOS; Linux and Windows users are encouraged to report compatibility feedback.

## Common setup

Install Go, Python 3.11 or newer, and [uv](https://docs.astral.sh/uv/getting-started/installation/). FFmpeg is optional and is only needed to convert audio into WhatsApp voice messages.

Local Whisper transcription is also optional. Install it from `whatsapp-mcp-server` with `uv sync --locked --extra whisper`, then launch the MCP server with `uv run --extra whisper main.py`. It uses faster-whisper/PyAV on macOS, Linux, and 64-bit Windows, downloads the configured model on first use, and does not require system FFmpeg. See the main README for model, disk, memory, and environment-variable requirements.

From the repository root, install the locked Python environment and run the bridge:

```console
cd whatsapp-mcp-server
uv sync --locked
cd ..
python3 scripts/run_bridge.py --instance-id personal
```

On Windows, use `py scripts/run_bridge.py --instance-id personal`. The launcher keeps state outside the checkout and prints the exact store path needed for `WHATSAPP_STORE_DIR`. Use `--print-store-dir` to print it without starting the bridge.

The bridge binds only to `127.0.0.1:8741`. Keep the bridge running before starting the MCP client.

The HTTP API has no network authentication. Keep it on loopback, do not put it
behind a reverse proxy or public tunnel, and do not expose it to a container or
shared-host network namespace you do not trust. MCP confirmation tokens protect
MCP workflows; they are not credentials for direct HTTP calls.

Every process should have a distinct instance ID. `/api/health` returns the ID
from `WHATSAPP_BRIDGE_INSTANCE_ID`, and startup checks should require the
expected value as well as `connected: true` and `logged_in: true`.

Resource defaults keep local AI reads and transcription finite: MCP page sizes
are capped at 100, page numbers at 1,000, and before/after context at 50 per
side. Bridge JSON bodies default to 1 MiB and inbound media downloads to 512
MiB; adjust `WHATSAPP_BRIDGE_MAX_REQUEST_BODY_BYTES` and
`WHATSAPP_BRIDGE_MAX_MEDIA_DOWNLOAD_BYTES` only when the host has an explicit
need and capacity. Invalid or non-positive overrides use the safe defaults.
Optional Whisper transcription defaults to 25 MiB, 15 minutes, 500 segments,
and 100,000 output characters. The corresponding settings are
`WHATSAPP_WHISPER_MAX_FILE_BYTES=26214400`,
`WHATSAPP_WHISPER_MAX_DURATION_SECONDS=900`,
`WHATSAPP_WHISPER_MAX_SEGMENTS=500`, and
`WHATSAPP_WHISPER_MAX_OUTPUT_CHARS=100000`. Check `whisper_status` for the
active transcription values before increasing them.

## Linux

The launcher follows the XDG data directory convention. Its default store is `$XDG_DATA_HOME/whatsapp-mcp/store` when that variable is set, otherwise `~/.local/share/whatsapp-mcp/store`. It creates or tightens the final store directory to `0700`. `xdg-open` is optional; without it, pairing falls back to a terminal QR code.

For a persistent user service, build the bridge and create a systemd unit:

```bash
mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user"
cd whatsapp-bridge
go build -o "$HOME/.local/bin/whatsapp-mcp-bridge" .
```

Save this as `~/.config/systemd/user/whatsapp-mcp-bridge.service`:

```ini
[Unit]
Description=WhatsApp MCP bridge
After=network-online.target

[Service]
Environment=WHATSAPP_BRIDGE_INSTANCE_ID=personal
ExecStart=%h/.local/bin/whatsapp-mcp-bridge --port 8741 --store-dir %h/.local/share/whatsapp-mcp/store --log-level WARN
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

Then enable and verify it:

```bash
systemctl --user daemon-reload
systemctl --user enable --now whatsapp-mcp-bridge
systemctl --user status whatsapp-mcp-bridge
curl -s http://127.0.0.1:8741/api/health
```

Confirm that health returns `instance_id: "personal"` in addition to connected
and logged-in status. Before the first service start, use
`install -d -m 700 "$HOME/.local/share/whatsapp-mcp/store"`; if migrating an
older store, ensure databases and private media are not group/world-readable.

Pair interactively before enabling the background service. If your distribution does not run user services until login, enable lingering with your system administrator only if that behavior is desired.

## Windows

The SQLite driver requires CGO. Install [MSYS2](https://www.msys2.org/), then in the MSYS2 UCRT64 terminal run:

```bash
pacman -S --needed mingw-w64-ucrt-x86_64-gcc
```

Add the MSYS2 `ucrt64\bin` directory (normally `C:\msys64\ucrt64\bin`) to your Windows `PATH`. In a new PowerShell window, verify and enable CGO:

```powershell
gcc --version
go env -w CGO_ENABLED=1
py scripts/run_bridge.py --instance-id personal
```

For startup at sign-in, first build a stable executable:

```powershell
$installDir = Join-Path $env:LOCALAPPDATA "whatsapp-mcp\bin"
New-Item -ItemType Directory -Force $installDir | Out-Null
Push-Location whatsapp-bridge
go build -o (Join-Path $installDir "whatsapp-mcp-bridge.exe") .
Pop-Location
```

Open **Task Scheduler > Create Task** and configure:

- Trigger: **At log on** for your user.
- Program: `%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe` (expand `%SystemRoot%` if Task Scheduler does not).
- Arguments: `-NoProfile -Command "$env:WHATSAPP_BRIDGE_INSTANCE_ID='personal'; $bridge=Join-Path $env:LOCALAPPDATA 'whatsapp-mcp\bin\whatsapp-mcp-bridge.exe'; $store=Join-Path $env:LOCALAPPDATA 'whatsapp-mcp\store'; & $bridge --port 8741 --store-dir $store --log-level WARN"`.
- Settings: enable restart after failure.

Pair interactively before enabling the task, use the same store directory in both commands, and verify that `/api/health` returns `instance_id: "personal"`. Keep the default user-only ACL on `%LOCALAPPDATA%\whatsapp-mcp`; do not grant broad read access to the store.

## macOS

The portable launcher works without Contacts permission. For a persistent LaunchAgent, stop the interactive bridge after pairing and run:

```bash
python3 scripts/install_macos_launch_agent.py
```

The installer tightens existing store/log directories to `0700`, copied private
files to `0600`, and binds the LaunchAgent label to the health `instance_id`
before reporting success. Add `--with-contacts` only if you want the optional
read-only Contacts.app integration. See [multi-account.md](multi-account.md) for isolated macOS profiles.

## MCP client config locations

Typical locations are:

| Client | macOS | Linux | Windows |
|---|---|---|---|
| Claude Desktop | `~/Library/Application Support/Claude/claude_desktop_config.json` | `~/.config/Claude/claude_desktop_config.json` | `%APPDATA%\Claude\claude_desktop_config.json` |
| Cursor | `~/.cursor/mcp.json` | `~/.cursor/mcp.json` | `%USERPROFILE%\.cursor\mcp.json` |

Use absolute paths in MCP configuration. In JSON on Windows, either use forward slashes or escape each backslash.
