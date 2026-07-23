import secrets
import threading
import time
from typing import Any, Dict, Optional, Tuple


CONFIRMATION_TTL_SECONDS = 10 * 60
_pending: Dict[str, Dict[str, Any]] = {}
_lock = threading.Lock()


def prepare(kind: str, recipient: str, payload: str) -> Dict[str, Any]:
    token = secrets.token_urlsafe(18)
    expires_at = time.time() + CONFIRMATION_TTL_SECONDS
    with _lock:
        _discard_expired_locked()
        _pending[token] = {
            "kind": kind,
            "recipient": recipient,
            "payload": payload,
            "expires_at": expires_at,
        }

    return {
        "success": False,
        "requires_confirmation": True,
        "confirmation_token": token,
        "expires_at_unix": int(expires_at),
        "kind": kind,
        "recipient": recipient,
        "preview": payload,
        "message": "Prepared only; nothing was sent. Ask the user to confirm this exact preview.",
    }


def consume(
    token: Optional[str], kind: str, recipient: str, payload: str
) -> Tuple[bool, str]:
    if not token:
        return False, "A confirmation token is required."

    with _lock:
        _discard_expired_locked()
        pending = _pending.pop(token, None)

    if pending is None:
        return False, "The confirmation token is invalid, expired, or already used."
    if (
        pending["kind"] != kind
        or pending["recipient"] != recipient
        or pending["payload"] != payload
    ):
        return False, "The confirmation token does not match this exact recipient and content."
    return True, "Confirmed"


def _discard_expired_locked() -> None:
    now = time.time()
    expired = [token for token, item in _pending.items() if item["expires_at"] <= now]
    for token in expired:
        _pending.pop(token, None)
