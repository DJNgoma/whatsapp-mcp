#!/usr/bin/env python3
"""Read-only verification for one isolated WhatsApp macOS profile."""

import argparse
import json
import os
import plistlib
import re
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Dict, Optional


LABEL_PREFIX = "com.djngoma.whatsapp-mcp-bridge"
DEFAULT_PROFILE = "personal"
DEFAULT_PORTS = {"personal": 8741, "company": 8742}
LAUNCH_AGENTS_DIR = Path.home() / "Library" / "LaunchAgents"


def normalize_profile(value: str) -> str:
    profile = value.strip().lower()
    if not re.fullmatch(r"[a-z0-9][a-z0-9.-]*", profile):
        raise ValueError("invalid profile name")
    return profile


def profile_label(profile: str) -> str:
    return LABEL_PREFIX if profile == DEFAULT_PROFILE else f"{LABEL_PREFIX}.{profile}"


def fetch_json(url: str) -> Dict[str, Any]:
    with urllib.request.urlopen(url, timeout=3) as response:
        payload = json.load(response)
    if not isinstance(payload, dict):
        raise RuntimeError(f"{url} returned a non-object response")
    return payload


def argument_value(arguments: list, name: str) -> Optional[str]:
    try:
        return arguments[arguments.index(name) + 1]
    except (ValueError, IndexError):
        return None


def verify(profile: str, port: int, expected_phone: str) -> Dict[str, Any]:
    label = profile_label(profile)
    plist_path = LAUNCH_AGENTS_DIR / f"{label}.plist"
    if not plist_path.exists():
        raise RuntimeError(f"LaunchAgent plist is missing: {plist_path}")

    with plist_path.open("rb") as plist_file:
        plist = plistlib.load(plist_file)
    arguments = plist.get("ProgramArguments", [])
    configured_port = argument_value(arguments, "--port")
    configured_store = argument_value(arguments, "--store-dir")
    if configured_port != str(port):
        raise RuntimeError(
            f"{label} is configured for port {configured_port}, expected {port}"
        )
    if not configured_store:
        raise RuntimeError(f"{label} has no --store-dir in its plist")

    loaded = subprocess.run(
        ["/bin/launchctl", "print", f"gui/{os.getuid()}/{label}"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    ).returncode == 0
    if not loaded:
        raise RuntimeError(f"{label} is not loaded in launchctl")

    base_url = f"http://127.0.0.1:{port}/api"
    health = fetch_json(f"{base_url}/health")
    if health.get("instance_id") != label:
        raise RuntimeError(
            f"{label} health check returned a different bridge instance"
        )
    identity_envelope = fetch_json(f"{base_url}/identity")
    identity = identity_envelope.get("data") or {}
    actual_phone = re.sub(r"\D", "", str(identity.get("phone_number", "")))
    expected_digits = re.sub(r"\D", "", expected_phone)
    if actual_phone != expected_digits:
        raise RuntimeError(
            f"{label} is connected as {actual_phone or '<unknown>'}, "
            f"expected {expected_digits}"
        )
    if not health.get("connected") or not health.get("logged_in"):
        raise RuntimeError(f"{label} is reachable but not connected/logged in")

    return {
        "profile": profile,
        "service": label,
        "port": port,
        "store_dir": configured_store,
        "health": health,
        "identity": identity,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--profile", default=DEFAULT_PROFILE)
    parser.add_argument("--port", type=int, default=None)
    parser.add_argument("--expected-phone", required=True)
    arguments = parser.parse_args()

    try:
        profile = normalize_profile(arguments.profile)
        port = (
            arguments.port
            if arguments.port is not None
            else DEFAULT_PORTS.get(profile)
        )
        if port is None:
            parser.error("--port is required for an unrecognized profile")
        result = verify(profile, port, arguments.expected_phone)
    except (OSError, RuntimeError, ValueError, urllib.error.URLError) as error:
        print(json.dumps({"ok": False, "error": str(error)}, indent=2))
        return 1

    print(json.dumps({"ok": True, **result}, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
