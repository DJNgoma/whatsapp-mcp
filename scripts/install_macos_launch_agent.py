#!/usr/bin/env python3
"""Build and install the WhatsApp bridge as a persistent macOS LaunchAgent."""

import argparse
import json
import os
import plistlib
import re
import shutil
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Optional


LABEL_PREFIX = "com.djngoma.whatsapp-mcp-bridge"
DEFAULT_PROFILE = "personal"
PROFILE_DEFAULT_PORTS = {
    "personal": 8741,
    "company": 8742,
}
REPOSITORY_ROOT = Path(__file__).resolve().parents[1]
BRIDGE_SOURCE = REPOSITORY_ROOT / "whatsapp-bridge"
CONTACTS_HELPER_SOURCE = (
    REPOSITORY_ROOT / "whatsapp-mcp-server" / "macos-contacts-helper" / "main.swift"
)
SOURCE_STORE = BRIDGE_SOURCE / "store"
DEFAULT_STATE_DIR = Path.home() / "Library" / "Application Support" / "whatsapp-mcp"
LAUNCH_AGENTS_DIR = Path.home() / "Library" / "LaunchAgents"


def normalize_profile(value: str) -> str:
    profile = value.strip().lower()
    if not re.fullmatch(r"[a-z0-9][a-z0-9.-]*", profile):
        raise ValueError(
            "profile must start with a letter or digit and contain only "
            "lowercase letters, digits, dots, or hyphens"
        )
    return profile


def profile_label(profile: str) -> str:
    return LABEL_PREFIX if profile == DEFAULT_PROFILE else f"{LABEL_PREFIX}.{profile}"


def profile_plist_path(profile: str) -> Path:
    return LAUNCH_AGENTS_DIR / f"{profile_label(profile)}.plist"


def default_state_dir(profile: str) -> Path:
    if profile == DEFAULT_PROFILE:
        return DEFAULT_STATE_DIR
    return DEFAULT_STATE_DIR / profile


def run(
    *arguments: str,
    check: bool = True,
    cwd: Optional[Path] = None,
) -> subprocess.CompletedProcess:
    return subprocess.run(arguments, check=check, cwd=cwd, text=True)


def service_target(profile: str) -> str:
    return f"gui/{os.getuid()}/{profile_label(profile)}"


def bootout_existing(profile: str) -> None:
    run("/bin/launchctl", "bootout", service_target(profile), check=False)


def backup_sqlite(source: Path, destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    with sqlite3.connect(source) as source_db, sqlite3.connect(destination) as destination_db:
        source_db.backup(destination_db)
    destination.chmod(0o600)


def harden_private_tree(root: Path) -> None:
    """Keep account state private even when reusing or copying an old store."""
    if not root.exists() or root.is_symlink():
        return
    root.chmod(0o700)
    for item in root.rglob("*"):
        if item.is_symlink():
            continue
        item.chmod(0o700 if item.is_dir() else 0o600)


def migrate_store(destination: Path, profile: str) -> None:
    destination.mkdir(parents=True, exist_ok=True, mode=0o700)
    harden_private_tree(destination)
    # The legacy checkout store is the personal account. Never seed another
    # profile from it: that would mix linked-device credentials and messages.
    if profile != DEFAULT_PROFILE:
        return
    if not SOURCE_STORE.exists() or SOURCE_STORE.resolve() == destination.resolve():
        return

    for database_name in ("whatsapp.db", "messages.db"):
        source_database = SOURCE_STORE / database_name
        destination_database = destination / database_name
        if source_database.exists() and not destination_database.exists():
            print(f"Migrating {database_name} to {destination}")
            backup_sqlite(source_database, destination_database)

    for source_item in SOURCE_STORE.iterdir():
        if source_item.name.endswith((".db", ".db-shm", ".db-wal")):
            continue
        destination_item = destination / source_item.name
        if source_item.is_dir():
            shutil.copytree(source_item, destination_item, dirs_exist_ok=True)
        elif not destination_item.exists():
            shutil.copy2(source_item, destination_item)
    harden_private_tree(destination)


def build_bridge(binary_path: Path) -> None:
    binary_path.parent.mkdir(parents=True, exist_ok=True)
    print(f"Building bridge at {binary_path}")
    run("/usr/bin/env", "go", "build", "-o", str(binary_path), ".", cwd=BRIDGE_SOURCE)
    binary_path.chmod(0o755)


def build_contacts_helper(state_dir: Path) -> Path:
    app_dir = state_dir / "bin" / "WhatsAppMCPContacts.app"
    contents_dir = app_dir / "Contents"
    executable_dir = contents_dir / "MacOS"
    executable_path = executable_dir / "whatsapp-mcp-contacts"
    if app_dir.exists():
        shutil.rmtree(app_dir)
    executable_dir.mkdir(parents=True, exist_ok=True)

    print(f"Building Contacts helper at {app_dir}")
    run(
        "/usr/bin/xcrun",
        "swiftc",
        "-framework",
        "Contacts",
        "-o",
        str(executable_path),
        str(CONTACTS_HELPER_SOURCE),
    )
    executable_path.chmod(0o755)
    info_plist = {
        "CFBundleDevelopmentRegion": "en",
        "CFBundleExecutable": executable_path.name,
        "CFBundleIdentifier": "com.djngoma.whatsapp-mcp.contacts",
        "CFBundleInfoDictionaryVersion": "6.0",
        "CFBundleName": "WhatsApp MCP Contacts",
        "CFBundlePackageType": "APPL",
        "CFBundleShortVersionString": "1.0",
        "CFBundleVersion": "1",
        "LSBackgroundOnly": True,
        "NSContactsUsageDescription": "Search your local contacts when resolving WhatsApp recipients.",
    }
    with (contents_dir / "Info.plist").open("wb") as plist_file:
        plistlib.dump(info_plist, plist_file)
    run(
        "/usr/bin/codesign",
        "--force",
        "--deep",
        "--sign",
        "-",
        "--identifier",
        "com.djngoma.whatsapp-mcp.contacts",
        str(app_dir),
    )
    return executable_path


def write_launch_agent(
    binary_path: Path,
    store_dir: Path,
    log_dir: Path,
    port: int,
    profile: str,
) -> None:
    label = profile_label(profile)
    plist_path = profile_plist_path(profile)
    LAUNCH_AGENTS_DIR.mkdir(parents=True, exist_ok=True)
    log_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    harden_private_tree(log_dir)
    configuration = {
        "Label": label,
        "ProgramArguments": [
            str(binary_path),
            "--port",
            str(port),
            "--store-dir",
            str(store_dir),
            "--log-level",
            "WARN",
        ],
        "WorkingDirectory": str(store_dir.parent),
        "EnvironmentVariables": {
            "WHATSAPP_BRIDGE_PORT": str(port),
            "WHATSAPP_BRIDGE_INSTANCE_ID": label,
            "WHATSAPP_STORE_DIR": str(store_dir),
        },
        "RunAtLoad": True,
        "KeepAlive": True,
        "ProcessType": "Background",
        "ThrottleInterval": 5,
        "StandardOutPath": str(log_dir / "bridge.log"),
        "StandardErrorPath": str(log_dir / "bridge.error.log"),
    }
    with plist_path.open("wb") as plist_file:
        plistlib.dump(configuration, plist_file)
    plist_path.chmod(0o600)
    run("/usr/bin/plutil", "-lint", str(plist_path))


def wait_for_health(
    port: int,
    expected_instance_id: str,
    timeout: int = 30,
) -> dict:
    health_url = f"http://127.0.0.1:{port}/api/health"
    deadline = time.monotonic() + timeout
    last_error: Optional[Exception] = None
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(health_url, timeout=2) as response:
                health = json.load(response)
            if not isinstance(health, dict):
                last_error = RuntimeError("bridge health response is not an object")
            elif health.get("instance_id") != expected_instance_id:
                last_error = RuntimeError(
                    "bridge health response belongs to a different instance"
                )
            elif not health.get("connected") or not health.get("logged_in"):
                last_error = RuntimeError("bridge is not connected and logged in")
            else:
                return health
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as error:
            last_error = error
        time.sleep(0.5)
    raise RuntimeError(f"Bridge health check failed at {health_url}: {last_error}")


def install(
    state_dir: Path,
    port: int,
    profile: str,
    with_contacts: bool = False,
) -> None:
    if sys.platform != "darwin":
        raise RuntimeError("The LaunchAgent installer is available only on macOS.")

    label = profile_label(profile)
    plist_path = profile_plist_path(profile)
    state_dir = state_dir.expanduser().resolve()
    store_dir = state_dir / "store"
    binary_path = state_dir / "bin" / "whatsapp-mcp-bridge"
    log_dir = state_dir / "logs"

    bootout_existing(profile)
    migrate_store(store_dir, profile)
    build_bridge(binary_path)
    contacts_helper_path = build_contacts_helper(state_dir) if with_contacts else None
    write_launch_agent(binary_path, store_dir, log_dir, port, profile)
    run("/bin/launchctl", "bootstrap", f"gui/{os.getuid()}", str(plist_path))
    run("/bin/launchctl", "kickstart", "-k", service_target(profile))

    health = wait_for_health(port, label)
    print(json.dumps(health, indent=2))
    print("\nInstalled persistent WhatsApp bridge.")
    print(f"Profile: {profile}")
    print(f"LaunchAgent: {plist_path}")
    print(f"Service: {label}")
    print(f"Store: {store_dir}")
    print(f"Logs: {log_dir}")
    if contacts_helper_path:
        print(f"Contacts helper: {contacts_helper_path}")
    else:
        print("Contacts helper: not installed (optional; rerun with --with-contacts)")
    print(f"MCP bridge URL: http://127.0.0.1:{port}/api")


def uninstall(profile: str) -> None:
    plist_path = profile_plist_path(profile)
    bootout_existing(profile)
    if plist_path.exists():
        plist_path.unlink()
    print(f"Removed {profile_label(profile)}; application data was preserved.")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--profile",
        default=DEFAULT_PROFILE,
        help="isolated account profile (personal or company)",
    )
    parser.add_argument("--port", type=int, default=None)
    parser.add_argument("--state-dir", type=Path, default=None)
    parser.add_argument(
        "--with-contacts",
        action="store_true",
        help="build the optional read-only macOS Contacts.app helper",
    )
    parser.add_argument("--uninstall", action="store_true")
    arguments = parser.parse_args()

    try:
        profile = normalize_profile(arguments.profile)
    except ValueError as error:
        parser.error(str(error))

    port = (
        arguments.port
        if arguments.port is not None
        else PROFILE_DEFAULT_PORTS.get(profile)
    )
    if port is None:
        parser.error("--port is required for an unrecognized profile")
    if not 1 <= port <= 65535:
        parser.error("--port must be between 1 and 65535")
    if arguments.uninstall:
        uninstall(profile)
    else:
        state_dir = arguments.state_dir or default_state_dir(profile)
        install(state_dir, port, profile, with_contacts=arguments.with_contacts)


if __name__ == "__main__":
    main()
