# WhatsApp multi-account setup

The bridge is single-account per process. Every WhatsApp account therefore needs its own bridge process, localhost port, store directory, and MCP server registration.

An example two-account layout is:

```text
personal account
  -> 127.0.0.1:8741/api
  -> <platform data directory>/whatsapp-mcp/store
  -> MCP server whatsapp-personal

work account
  -> 127.0.0.1:8742/api
  -> <platform data directory>/whatsapp-mcp/work/store
  -> MCP server whatsapp-work
```

Never run two bridge processes against the same store.

## Pair a second account

Create a separate store and choose a unique port. On macOS or Linux:

```bash
python3 scripts/run_bridge.py \
  --instance-id work \
  --phone +15551234567 \
  --port 8742 \
  --store-dir "$HOME/.local/share/whatsapp-mcp/work/store"
```

On macOS, use `"$HOME/Library/Application Support/whatsapp-mcp/work/store"` instead. On Windows PowerShell:

```powershell
py scripts/run_bridge.py `
  --instance-id work `
  --phone +15551234567 `
  --port 8742 `
  --store-dir "$env:LOCALAPPDATA\whatsapp-mcp\work\store"
```

The `--phone` option creates a linked-device pairing code; it does not move or convert another session. Omit it to use the newest QR image opened by the system viewer or shown in the terminal.

After pairing, verify the second lane:

```bash
curl -sS http://127.0.0.1:8742/api/health
curl -sS http://127.0.0.1:8742/api/identity
```

Health must report `instance_id: "work"`, `connected: true`, and `logged_in: true`. Confirm that `/api/identity` contains the intended account before performing any outbound operation.

## Persistent services

Replicate the service pattern in [platform-setup.md](platform-setup.md) with the second port and store.

On macOS, the profile-aware installer provides an isolated `company` profile on port `8742`:

```bash
python3 scripts/install_macos_launch_agent.py --profile company
python3 scripts/verify_macos_profile.py \
  --profile company \
  --expected-phone +15551234567
```

To remove only that service while preserving its data:

```bash
python3 scripts/install_macos_launch_agent.py --profile company --uninstall
```

## Register separate MCP servers

Register a distinct MCP server name for every account. Each entry uses the same `uv` command and MCP source directory, but a different `WHATSAPP_STORE_DIR` and `WHATSAPP_BRIDGE_URL`.

```bash
codex mcp add \
  --env 'WHATSAPP_STORE_DIR=/absolute/path/to/personal/store' \
  --env 'WHATSAPP_BRIDGE_URL=http://127.0.0.1:8741/api' \
  whatsapp-personal -- \
  /absolute/path/to/uv \
  --directory /absolute/path/to/whatsapp-mcp/whatsapp-mcp-server \
  run main.py

codex mcp add \
  --env 'WHATSAPP_STORE_DIR=/absolute/path/to/work/store' \
  --env 'WHATSAPP_BRIDGE_URL=http://127.0.0.1:8742/api' \
  whatsapp-work -- \
  /absolute/path/to/uv \
  --directory /absolute/path/to/whatsapp-mcp/whatsapp-mcp-server \
  run main.py
```

For Claude Desktop or Cursor, create two entries with the same command and distinct environment pairs.

## Safety rules

- Keep every bridge bound to `127.0.0.1`; do not expose it to the LAN or internet.
- Never copy `whatsapp.db`, `messages.db`, or downloaded media between account stores.
- Before sending, call `get_own_identity` through the account-specific MCP server and verify the selected account.
- Keep instance IDs, profile labels, ports, stores, and MCP names aligned. A healthy bridge with the wrong `instance_id` or identity is still the wrong account.
- If one profile logs out, repair only that profile and preserve its `messages.db` unless a full history reset is intentional.
