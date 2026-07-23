import json
import os
import platform
import subprocess
from pathlib import Path
from typing import Any, Dict, List, Tuple


DEFAULT_HELPER_PATH = (
    Path.home()
    / "Library"
    / "Application Support"
    / "whatsapp-mcp"
    / "bin"
    / "WhatsAppMCPContacts.app"
    / "Contents"
    / "MacOS"
    / "whatsapp-mcp-contacts"
)
HELPER_PATH = Path(os.environ.get("WHATSAPP_CONTACTS_HELPER", DEFAULT_HELPER_PATH))


def _run_contacts_helper(*arguments: str, timeout: int = 30) -> Any:
    completed = subprocess.run(
        [str(HELPER_PATH), *arguments],
        check=True,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    return json.loads(completed.stdout)


def _error_message(error: Exception) -> str:
    if isinstance(error, subprocess.CalledProcessError):
        detail = (error.stderr or error.stdout or str(error)).strip()
    else:
        detail = str(error)

    if "permission_denied" in detail.lower():
        return (
            "Contacts access is denied. Open System Settings > Privacy & Security > "
            "Contacts and enable WhatsApp MCP Contacts."
        )
    return detail


def contacts_app_status() -> Dict[str, Any]:
    if platform.system() != "Darwin":
        return {
            "available": False,
            "authorized": False,
            "message": "Contacts.app lookup is available only on macOS.",
        }
    if not HELPER_PATH.is_file():
        return {
            "available": False,
            "authorized": False,
            "message": (
                "The optional Contacts.app integration is not installed. "
                "Run scripts/install_macos_launch_agent.py --with-contacts to enable it."
            ),
            "helper_path": str(HELPER_PATH),
        }

    try:
        result = _run_contacts_helper("--probe")
        return {
            "available": True,
            "authorized": bool(result.get("authorized")),
            "authorization_status": result.get("authorization_status"),
            "message": result.get("message", "Contacts helper is available."),
            "helper_path": str(HELPER_PATH),
        }
    except (subprocess.SubprocessError, OSError, ValueError, json.JSONDecodeError) as error:
        return {
            "available": True,
            "authorized": False,
            "message": _error_message(error),
            "helper_path": str(HELPER_PATH),
        }


def search_macos_contacts(query: str, limit: int = 50) -> Tuple[List[Dict[str, Any]], str | None]:
    if platform.system() != "Darwin":
        return [], "Contacts.app lookup is available only on macOS."
    if not HELPER_PATH.is_file():
        return [], "The native Contacts helper is not installed."

    try:
        result = _run_contacts_helper(query, str(limit))
        if not isinstance(result, list):
            return [], "Contacts.app returned an unexpected response."
        return result, None
    except (subprocess.SubprocessError, OSError, ValueError, json.JSONDecodeError) as error:
        return [], _error_message(error)
