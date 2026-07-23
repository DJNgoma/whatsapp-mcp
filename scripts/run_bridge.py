#!/usr/bin/env python3
"""Run the WhatsApp bridge with a platform-appropriate persistent store."""

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path


REPOSITORY_ROOT = Path(__file__).resolve().parents[1]
BRIDGE_SOURCE = REPOSITORY_ROOT / "whatsapp-bridge"
DEFAULT_INSTANCE_ID = "portable"


def default_state_dir(
    platform: str = sys.platform,
    environment: dict[str, str] = os.environ,
    home: Path | None = None,
) -> Path:
    home = home or Path.home()
    if platform == "darwin":
        return home / "Library" / "Application Support" / "whatsapp-mcp"
    if platform == "win32":
        base = environment.get("LOCALAPPDATA") or environment.get("APPDATA")
        return Path(base) / "whatsapp-mcp" if base else home / "AppData" / "Local" / "whatsapp-mcp"
    data_home = environment.get("XDG_DATA_HOME")
    return (Path(data_home) if data_home else home / ".local" / "share") / "whatsapp-mcp"


def normalize_instance_id(value: str) -> str:
    instance_id = value.strip()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", instance_id):
        raise ValueError(
            "instance ID must start with a letter or digit, be at most 128 characters, "
            "and contain only letters, digits, dots, underscores, or hyphens"
        )
    return instance_id


def ensure_private_store_directory(
    store_dir: Path,
    platform: str = sys.platform,
) -> None:
    store_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    if platform != "win32":
        # mkdir(mode=...) does not tighten an existing directory. Apply the
        # invariant explicitly before the bridge creates credentials or media.
        store_dir.chmod(0o700)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Run the Go bridge with a persistent store outside the Git checkout."
    )
    parser.add_argument("--store-dir", type=Path, default=default_state_dir() / "store")
    parser.add_argument("--port", type=int, default=8741)
    parser.add_argument("--phone", help="pair with an international phone number instead of QR")
    parser.add_argument("--log-level", default="INFO", choices=("DEBUG", "INFO", "WARN", "ERROR"))
    parser.add_argument("--log-messages", action="store_true")
    parser.add_argument(
        "--instance-id",
        default=os.environ.get("WHATSAPP_BRIDGE_INSTANCE_ID", DEFAULT_INSTANCE_ID),
        help="stable identifier exposed by /api/health for profile verification",
    )
    parser.add_argument("--print-store-dir", action="store_true")
    arguments = parser.parse_args()

    store_dir = arguments.store_dir.expanduser().resolve()
    if arguments.print_store_dir:
        print(store_dir)
        return 0

    try:
        instance_id = normalize_instance_id(arguments.instance_id)
    except ValueError as error:
        parser.error(str(error))

    ensure_private_store_directory(store_dir)
    command = [
        "go", "run", ".",
        "--port", str(arguments.port),
        "--store-dir", str(store_dir),
        "--log-level", arguments.log_level,
    ]
    if arguments.phone:
        command.extend(("--phone", arguments.phone))
    if arguments.log_messages:
        command.append("--log-messages")

    child_environment = os.environ.copy()
    child_environment["WHATSAPP_BRIDGE_INSTANCE_ID"] = instance_id

    print(f"WhatsApp store: {store_dir}", flush=True)
    try:
        return subprocess.run(
            command,
            cwd=BRIDGE_SOURCE,
            check=False,
            env=child_environment,
        ).returncode
    except FileNotFoundError:
        print("Go was not found on PATH. Install Go and retry.", file=sys.stderr)
        return 127


if __name__ == "__main__":
    raise SystemExit(main())
