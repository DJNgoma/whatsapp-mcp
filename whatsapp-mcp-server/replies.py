"""Quoted replies: preview and bind confirmation to an exact stored target."""
import json
from typing import Any, Dict, Optional

from outbound_confirmation import consume, prepare
from whatsapp import _bridge_api_request


def reply_message(chat_jid: str, message_id: str, message: str,
                  confirmation_token: Optional[str] = None) -> Dict[str, Any]:
    """Prepare, then confirm a text reply quoting a message in the exact chat.

    Copy chat_jid and message_id from list_messages. Phone numbers are accepted
    for phone-addressed direct chats; use the stored @lid JID for LID chats.
    Preview includes the original sender and quoted text/media type. Show this
    and the reply to the user before confirming. Repeat all arguments unchanged
    with the returned one-time confirmation_token after explicit approval.
    Old media without a captured payload and ambiguous legacy group senders
    fail closed. Never silently fall back to an unquoted send.
    """
    if not chat_jid.strip() or not message_id.strip() or not message.strip():
        return {"success": False, "message": "chat_jid, message_id and message are required"}
    result = _bridge_api_request("GET", "reply/preview", params={
        "chat_jid": chat_jid, "message_id": message_id,
    })
    if not result.get("success"):
        return result
    target = result["data"]
    payload = json.dumps({"message": message, "quoted_message": target},
                         sort_keys=True, ensure_ascii=False, separators=(",", ":"))
    if not confirmation_token:
        preview = prepare("reply", target["chat_jid"], payload)
        preview["quoted_message"] = target
        preview["reply_text"] = message
        return preview
    confirmed, error = consume(confirmation_token, "reply", target["chat_jid"], payload)
    if not confirmed:
        return {"success": False, "message": error}
    result = _bridge_api_request("POST", "reply", payload={
        "chat_jid": target["chat_jid"], "message_id": target["message_id"],
        "message": message, "fingerprint": target["fingerprint"],
    })
    result["requires_confirmation"] = False
    return result
