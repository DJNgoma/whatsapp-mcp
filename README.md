# WhatsApp MCP Server

This is a Model Context Protocol (MCP) server for WhatsApp.

With this server, an MCP client can search and read your WhatsApp messages (including images, videos, documents, and audio), search contacts, and send messages to individuals or groups. It can also send media files.

It connects to one WhatsApp account at a time through WhatsApp's web multi-device API using [whatsmeow](https://github.com/tulir/whatsmeow). Messages are stored locally in SQLite and are returned to an AI client only when it invokes the corresponding MCP tools.

The core bridge and MCP server are designed for macOS, Linux, and Windows. For two or more accounts, run one isolated bridge profile per account. See [platform setup](docs/platform-setup.md) and the [multi-account guide](docs/multi-account.md).

## Fork and attribution

This repository is a maintained fork of [Luke Harries' original `whatsapp-mcp` project](https://github.com/lharries/whatsapp-mcp). Credit for the original architecture and implementation belongs to Luke Harries and the [upstream contributors](https://github.com/lharries/whatsapp-mcp/graphs/contributors). The original MIT copyright notice is preserved in [LICENSE](LICENSE).

### Improvements in this fork

- Supports isolated personal and work accounts with separate stores, ports, identities, and MCP registrations.
- Adds explicit confirmation before messages, files, group changes, chat-state changes, blocking, or typing-presence actions are executed.
- Expands group, account-identity, contact-resolution, chat-management, media, call-history, and targeted history-synchronization capabilities.
- Adds delivery/read receipt tracking and a bounded, replayable Server-Sent Events stream for live messages and receipts.
- Improves pairing with native QR image handling, phone-number pairing, clearer linked-device identities, and terminal fallback.
- Provides persistent-service guidance and platform-native data locations for macOS, Linux, and Windows; macOS Contacts.app access remains optional.
- Hardens media handling with collision-resistant downloads, portable filenames, correct document MIME metadata, and WhatsApp `FileName` support.
- Adds locked Python dependencies, automated tests, race/static checks, and CI coverage across macOS, Linux, and Windows.

> *Caution:* as with many MCP servers, WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). Prompt injection could therefore lead to private data exfiltration. Keep the bridge bound to localhost, review tool calls, and require confirmation for outbound actions.

## Installation

### Prerequisites

- Go
- Python 3.11+
- An MCP client such as Codex, Claude Desktop, or Cursor
- [uv](https://docs.astral.sh/uv/) (Python package and environment manager)
- FFmpeg (optional) — needed only to convert non-Opus audio into playable WhatsApp voice messages. Without FFmpeg, raw audio can still be sent with `send_file`.
- Whisper transcription (optional) — installed separately through the `whisper` dependency extra described below.

### Steps

1. **Clone this repository**

   ```bash
   git clone https://github.com/DJNgoma/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Install UV and prepare the Python MCP server**

   UV creates an isolated Python environment, installs the exact dependencies recorded in `uv.lock`, and launches the stdio MCP server for your MCP client. It manages the Python MCP component only; the Go WhatsApp bridge still runs separately.

   Install UV using the [official instructions](https://docs.astral.sh/uv/getting-started/installation/). Common commands are:

   ```bash
   # macOS with Homebrew
   brew install uv

   # macOS or Linux
   curl -LsSf https://astral.sh/uv/install.sh | sh
   ```

   ```powershell
   # Windows PowerShell
   powershell -ExecutionPolicy ByPass -c "irm https://astral.sh/uv/install.ps1 | iex"
   ```

   Open a new terminal if the installer updates your `PATH`, then verify UV and prepare the locked environment:

   ```bash
   uv --version
   cd whatsapp-mcp-server
   uv sync --locked
   cd ..
   ```

   `uv sync --locked` creates `whatsapp-mcp-server/.venv` and fails rather than silently changing the dependency lockfile.

   To enable local speech-to-text, install the optional [faster-whisper](https://github.com/SYSTRAN/faster-whisper) backend instead:

   ```bash
   cd whatsapp-mcp-server
   uv sync --locked --extra whisper
   cd ..
   ```

   Whisper requirements and behavior:

   - Python 3.11 or newer on macOS, Linux, or 64-bit Windows.
   - Internet access on first use to download the selected model. The default multilingual `base` model is a practical CPU-friendly starting point; model weights are cached by the Hugging Face client.
   - Allow roughly 1 GB of free disk space and at least 2 GB of available RAM for the default model, dependencies, cache, and working data. Larger models require substantially more.
   - A CPU is sufficient. NVIDIA GPU acceleration is optional and requires a CUDA/cuDNN combination supported by the installed CTranslate2 release.
   - System FFmpeg is not required for transcription because faster-whisper decodes audio through PyAV. FFmpeg remains useful for converting outbound voice messages.
   - Audio is transcribed locally. It is not uploaded to an external transcription API, although model weights are downloaded on first use.

   Optional environment variables are `WHATSAPP_WHISPER_MODEL` (default `base`), `WHATSAPP_WHISPER_DEVICE` (default `auto`), and `WHATSAPP_WHISPER_COMPUTE_TYPE` (default `default`). Resource caps are `WHATSAPP_WHISPER_MAX_FILE_BYTES=26214400`, `WHATSAPP_WHISPER_MAX_DURATION_SECONDS=900`, `WHATSAPP_WHISPER_MAX_SEGMENTS=500`, and `WHATSAPP_WHISPER_MAX_OUTPUT_CHARS=100000`. Restart the MCP client after installing or changing these values. In the MCP client command, use `run --extra whisper main.py` instead of `run main.py` so uv always activates the optional dependency when launching the server.

3. **Run and pair the WhatsApp bridge**

   The portable launcher chooses the native per-user data directory, prints it, and starts the bridge on localhost port `8741`:

   ```console
   # macOS or Linux
   python3 scripts/run_bridge.py --instance-id personal

   # Windows PowerShell
   py scripts/run_bridge.py --instance-id personal
   ```

   The stores default to:

   - macOS: `~/Library/Application Support/whatsapp-mcp/store`
   - Linux: `$XDG_DATA_HOME/whatsapp-mcp/store` when set, otherwise `~/.local/share/whatsapp-mcp/store`
   - Windows: `%LOCALAPPDATA%\whatsapp-mcp\store`

   On macOS and Linux, the launcher creates or tightens the store directory to
   mode `0700` before the bridge writes linked-device state, messages, or media.
   Windows uses the current user's application-data ACL instead of POSIX modes.
   Give every concurrently running profile a distinct `--instance-id`; the
   bridge returns it from `/api/health` so service checks can distinguish a
   healthy process from a different account already using the port.

   The first time you run it, the bridge writes the QR code to a temporary PNG and opens it automatically in your system image viewer. Current WhatsApp pairing uses two scans:

   1. Scan the newest QR image with your phone's regular Camera app.
   2. Tap the WhatsApp link that appears, then tap **Continue** in WhatsApp.
   3. Scan the newest QR image again inside WhatsApp to confirm linking.

   WhatsApp rotates pairing codes, so always use the newest image. If the system image viewer cannot be opened, the QR code is displayed in the terminal as a fallback.

   New pairings use the current host platform for their linked-device identity. Existing linked-device labels cannot be changed in place.

   Alternatively, pair using your phone number in international format:

   ```console
   python3 scripts/run_bridge.py --instance-id personal --phone +15551234567
   # Windows: py scripts/run_bridge.py --instance-id personal --phone +15551234567
   ```

   The bridge prints an eight-character code. On your phone, open **WhatsApp > Settings > Linked Devices > Link a Device > Link with phone number instead**, then enter it. Omit `--phone` to use QR pairing.

   Keep this process running while using the MCP server. For background startup, Linux systemd and Windows Task Scheduler instructions are in [docs/platform-setup.md](docs/platform-setup.md). On macOS, you can install the included LaunchAgent after pairing:

   ```bash
   python3 scripts/install_macos_launch_agent.py
   ```

   The installer derives an instance ID from the profile's LaunchAgent label
   and does not report success until `/api/health` returns that exact ID while
   connected and logged in. A different bridge on the same port therefore
   fails the install check instead of being mistaken for the requested profile.

   The optional Contacts.app helper remains macOS-only; WhatsApp contact/chat search works on every supported platform without it. Opt in on macOS with `python3 scripts/install_macos_launch_agent.py --with-contacts`.

4. **Connect your MCP client**

   Get the store directory selected by the launcher, plus the absolute paths required by MCP clients:

   ```bash
   python3 scripts/run_bridge.py --print-store-dir
   command -v uv
   cd whatsapp-mcp-server && pwd
   ```

   On Windows, use `py scripts/run_bridge.py --print-store-dir`, `Get-Command uv`, and `(Resolve-Path whatsapp-mcp-server).Path` in PowerShell.

   For **Codex** on macOS or Linux, register the server using the absolute paths returned above:

   ```bash
   codex mcp add \
     --env 'WHATSAPP_STORE_DIR=/absolute/path/printed/above' \
     --env 'WHATSAPP_BRIDGE_URL=http://127.0.0.1:8741/api' \
     whatsapp -- \
     /absolute/path/to/uv \
     --directory /absolute/path/to/whatsapp-mcp/whatsapp-mcp-server \
     run main.py
   ```

   Verify the registration:

   ```bash
   codex mcp get whatsapp
   ```

   On Windows, use PowerShell with the paths returned by `Get-Command uv` and `Resolve-Path`:

   ```powershell
   codex mcp add `
     --env "WHATSAPP_STORE_DIR=C:/Users/your-name/AppData/Local/whatsapp-mcp/store" `
     --env "WHATSAPP_BRIDGE_URL=http://127.0.0.1:8741/api" `
     whatsapp -- `
     "C:/absolute/path/to/uv.exe" `
     --directory "C:/absolute/path/to/whatsapp-mcp/whatsapp-mcp-server" `
     run main.py
   ```

   Forward slashes are valid in these Windows command paths and avoid backslash escaping.

   For **Claude Desktop** or **Cursor**, use the same absolute paths in this configuration:

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "/absolute/path/to/uv",
         "env": {
           "WHATSAPP_STORE_DIR": "/absolute/path/printed/above",
           "WHATSAPP_BRIDGE_URL": "http://127.0.0.1:8741/api"
         },
         "args": [
           "--directory",
           "/absolute/path/to/whatsapp-mcp/whatsapp-mcp-server",
           "run",
           "main.py"
         ]
       }
     }
   }
   ```

   Use forward slashes or escaped backslashes (`\\`) in Windows JSON paths. Platform-specific Claude Desktop and Cursor config locations are listed in [docs/platform-setup.md](docs/platform-setup.md).

5. **Restart your MCP client and test**

   Restart Codex, Claude Desktop, or Cursor so it loads the newly registered server. For a safe first test, ask:

   > Use WhatsApp to list my five most recent chats. Do not send anything.

### Platform support

CI is configured to build and test the Go bridge and Python MCP server on macOS, Ubuntu Linux, and Windows. Live account testing for this fork has been performed on macOS; Linux and Windows still need broader real-world validation, so bug reports and compatibility feedback are appreciated. Windows needs CGO plus a C compiler because the bridge uses SQLite; the exact MSYS2 setup and persistent-service instructions are in [docs/platform-setup.md](docs/platform-setup.md).

## Architecture overview

This application consists of two main components:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): A Go application that connects to WhatsApp's web API, handles authentication via QR code, and stores message history in SQLite. It serves as the bridge between WhatsApp and the MCP server.

2. **Python MCP server** (`whatsapp-mcp-server/`): A Python server implementing the Model Context Protocol (MCP), which exposes standardized WhatsApp tools to compatible AI clients.

### Data storage

- All message history, login state, and downloaded media are stored locally in the configured store directory
- Each account profile must have its own store directory, bridge port, service/process, and MCP process; never point two accounts at the same SQLite store
- The portable launcher uses each platform's per-user application-data directory; `whatsapp-bridge/store` remains the legacy direct-run default
- Portable macOS/Linux launchers and the macOS installer enforce `0700` account-state and log directories; migrated databases and copied private files are tightened to `0600`
- The database maintains tables for chats and messages
- Messages are indexed for efficient searching and retrieval

### Bridge API notes

The bridge API is intentionally unauthenticated and bound only to `127.0.0.1`.
Do not bind it to all interfaces, expose it through a reverse proxy, publish it
through a tunnel, or treat the MCP confirmation token as HTTP authentication.
Any local process able to reach the port can call the API directly. Use a
distinct port, store, process, MCP registration, and health `instance_id` for
every account profile.

The bridge exposes paginated `/api/chats` and `/api/messages` endpoints using `limit` and `offset`, returns `phone_number` where a direct chat can be resolved, and publishes live messages and receipts through the replayable `GET /api/events` Server-Sent Events stream. SSE clients should reconnect with `Last-Event-ID`; an `event: reset` response means the cursor fell outside the bounded replay window and the client should reconcile through the read endpoints.

Message reads union a direct chat's phone JID and LID aliases. Media captions and call records are retained, and local media filenames include the message ID to prevent collisions. The targeted `POST /api/history-sync` endpoint requests older or newer history from a stored message anchor; it cannot manufacture records WhatsApp does not return.

## Usage

Once connected, Codex, Claude Desktop, Cursor, or another compatible MCP client can use the tools below.

### MCP tools

The connected MCP client can access these WhatsApp tools:

- **search_contacts**: Search for contacts by name or phone number
- **contacts_app_status**: Check macOS Contacts.app availability and permission
- **bridge_status**: Report live bridge connectivity and whether database reads may be stale
- **get_own_identity**: Return the connected WhatsApp JID, phone number, display name, and device identity
- **search_contacts_chats_and_groups**: Search direct chats, groups, recent chats, and optional macOS Contacts.app entries
- **list_messages**: Retrieve messages with optional filters and context
- **list_chats**: List available chats with metadata
- **list_joined_groups**: List live WhatsApp groups and participant counts
- **get_group_info**: Read live group metadata, participants, and admin roles
- **create_group**: Prepare and explicitly confirm creation of a WhatsApp group
- **add_participants_to_group** / **remove_participants_from_group**: Prepare and explicitly confirm membership changes
- **promote_participants_to_admins** / **demote_participants_from_admins**: Prepare and explicitly confirm admin-role changes
- **set_chat_read_state**: Prepare and explicitly confirm marking a chat read or unread
- **is_on_whatsapp**: Validate phone numbers against WhatsApp without sending
- **set_chat_mute** / **set_chat_archive**: Prepare and explicitly confirm chat-state changes
- **set_contact_block**: Prepare and explicitly confirm blocking or unblocking a contact
- **send_typing_indicator**: Prepare and explicitly confirm composing, recording, or paused presence
- **get_chat**: Get information about a specific chat
- **get_direct_chat_by_contact**: Find a direct chat with a specific contact
- **get_contact_chats**: List all chats involving a specific contact
- **get_last_interaction**: Get the most recent message with a contact
- **get_message_context**: Retrieve context around a specific message
- **send_message**: Prepare and explicitly confirm a WhatsApp message to a phone number or group JID
- **send_file**: Prepare and explicitly confirm a file send (image, video, raw audio, document)
- **send_audio_message**: Prepare and explicitly confirm an audio send (requires an `.ogg` Opus file or FFmpeg)
- **download_media**: Download media from a WhatsApp message and get the local file path
- **whisper_status**: Report whether optional local Whisper transcription is installed and show the selected model/device settings
- **transcribe_audio_message**: Download a WhatsApp audio message and transcribe it locally, with optional language and vocabulary hints

### Media handling

The MCP server supports both sending and receiving various media types:

#### Media sending

You can send various media types to your WhatsApp contacts:

- **Images, Videos, Documents**: Use the `send_file` tool to share any supported media type.
- **Voice Messages**: Use the `send_audio_message` tool to send audio files as playable WhatsApp voice messages.
  - For optimal compatibility, audio files should be in `.ogg` Opus format.
  - With FFmpeg installed, the system will automatically convert other audio formats (MP3, WAV, etc.) to the required format.
  - Without FFmpeg, you can still send raw audio files using the `send_file` tool, but they won't appear as playable voice messages.
- **Document metadata**: Document MIME types and filenames are preserved so files such as PDFs arrive with readable names and the correct type.

#### Media downloading

By default, the database stores media metadata rather than the complete file. Use `download_media` with the message's `message_id` and `chat_jid` to download it. The tool returns a platform-native local path that can be opened or passed to another tool.

#### Optional Whisper transcription

After installing the `whisper` extra, use `transcribe_audio_message` with the same `message_id` and `chat_jid` shown by message tools. The first request downloads the configured model and can therefore take longer. Subsequent requests reuse the cached model in the MCP process. Use `whisper_status` to diagnose installation or configuration without downloading a model.

## Technical details

1. The AI client invokes a tool on the Python MCP server.
2. Read tools query the local SQLite cache; live operations call the Go bridge.
3. The Go bridge communicates with WhatsApp and keeps the local cache up to date.
4. Results return through the MCP server to the client.
5. Outbound and state-changing operations require the confirmation flow described below before reaching WhatsApp.

## Operational and safety boundaries

- Read tools query the local `messages.db`. Use `bridge_status` to determine whether the bridge is connected; results may be stale while it is stopped or disconnected.
- Sending and media downloads require the live bridge. The default endpoint is localhost-only at `http://127.0.0.1:8741/api`.
- Contact search can optionally merge known WhatsApp direct chats with read-only Contacts.app results on macOS. Enable it with `python3 scripts/install_macos_launch_agent.py --with-contacts`. Contacts without a known chat are marked `has_whatsapp_chat: false`; the server does not assume that every address-book number has WhatsApp.
- Contact-search tools require a non-empty name or phone-number fragment; blank queries return no contacts.
- Contacts.app access is opt-in. On first use, macOS asks whether **WhatsApp MCP Contacts** may access your contacts. If the helper is absent or permission is denied, `contacts_app_status` reports that state and WhatsApp-only contact and chat search continues to work.
- Text, file, and audio sends use a two-stage flow. The first tool call returns a preview and a short-lived one-time token without sending. A second matching call is required after explicit user confirmation.
- Persistent service logs run at WARN level and omit message bodies by default. Interactive debugging can opt in with `--log-messages`, but this writes private content to the console or configured log.
- Group, read-state, mute, archive, block, and typing operations use the same exact-preview confirmation contract as outbound messages.
- Bridge JSON request bodies default to a 1 MiB cap (`WHATSAPP_BRIDGE_MAX_REQUEST_BODY_BYTES`), and inbound media downloads default to 512 MiB (`WHATSAPP_BRIDGE_MAX_MEDIA_DOWNLOAD_BYTES`). Invalid or non-positive overrides fall back to those defaults; lower the values on resource-constrained machines.
- MCP message/chat reads cap page sizes at 100, page numbers at 1,000, and per-side message context at 50. Invalid or excessive values are clamped again in the database helpers so negative SQLite limits cannot expand into full-store reads.
- Local Whisper transcription defaults to a 25 MiB input, 15 minutes of decoded audio, 500 segments, and 100,000 output characters. `whisper_status` reports the active limits; the `WHATSAPP_WHISPER_MAX_*` environment variables may lower or deliberately raise them for a trusted workstation.
- The MCP currently cannot edit or delete messages, react, post statuses, make calls, or manage product-level lists and workflows.

## Troubleshooting

- If you encounter permission issues when running `uv`, add it to `PATH` or use its absolute path. Use `command -v uv` on macOS/Linux or `Get-Command uv` in Windows PowerShell.
- Use `bridge_status` or the `/api/health` endpoint to distinguish a stopped bridge from a stale local database.
- For service logs and startup checks on macOS, Linux, and Windows, use the matching section in the [platform setup guide](docs/platform-setup.md).

### Authentication issues

- **QR code not displayed**: Restart the bridge launcher. On macOS it uses the default image viewer, on Linux it uses `xdg-open`, and on Windows it uses the registered image handler. If opening the image fails, use the QR code printed in the terminal.
- **WhatsApp already logged in**: If your session is already active, the Go bridge reconnects without showing a QR code.
- **Device limit reached**: WhatsApp limits the number of linked devices. Remove an unused device from **WhatsApp > Settings > Linked Devices**, then retry.
- **No messages loading**: Initial history synchronization can take several minutes, especially for accounts with many chats.
- **Partial history after recovery**: Use the bridge's targeted history-sync endpoint with a stored oldest-message anchor. If WhatsApp returns no earlier records, the bridge cannot fill the gap; inspect logs and retain the local store.
- **WhatsApp out of sync**: Stop the bridge, unlink the affected linked device in WhatsApp, and move that profile's `whatsapp.db` aside before pairing again with the same store. Preserve `messages.db`; it contains local message history. Never repair one account using another account's store.

For client integration help, see the [Codex MCP documentation](https://developers.openai.com/codex/mcp), the [MCP guide for connecting local servers](https://modelcontextprotocol.io/docs/develop/connect-local-servers) (including Claude Desktop), or the [Cursor MCP documentation](https://docs.cursor.com/context/model-context-protocol). These client instructions apply across macOS, Linux, and Windows where the respective client is available; bridge-specific platform commands remain in the [platform setup guide](docs/platform-setup.md).
