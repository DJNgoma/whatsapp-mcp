"""Recipient receipt verification. A pending receipt never triggers a resend."""
from typing import Any, Dict, Optional, Tuple
from whatsapp import _bridge_api_request


def get_message_delivery_status(chat_jid: str, message_id: str, wait_seconds: int = 0) -> Dict[str, Any]:
    """Check an exact outbound message; optionally wait up to 30 seconds.

    Read-only: never sends or retries. Delivery is confirmed only by a recorded
    recipient delivery/read receipt. A group-member receipt is partial only.
    Copy chat_jid/message_id from the send result. Offline devices may remain
    pending indefinitely; report that uncertainty rather than claiming delivery.
    """
    if not chat_jid.strip() or not message_id.strip():
        return {"success": False, "delivery_confirmed": False, "message": "chat_jid and message_id are required"}
    if type(wait_seconds) is not int or not 0 <= wait_seconds <= 30:
        return {"success": False, "delivery_confirmed": False, "message": "wait_seconds must be 0-30"}
    return _bridge_api_request("GET", "message-status", params={
        "chat_jid": chat_jid, "message_id": message_id, "wait_seconds": wait_seconds,
    }, timeout=wait_seconds + 5)


def finish_send(recipient: str, sent: Tuple[bool, str, Optional[str]]) -> Dict[str, Any]:
    success, message, message_id = sent
    result = {"success": success, "requires_confirmation": False,
              "message_id": message_id, "chat_jid": recipient,
              "send_status": "accepted" if success else "unknown",
              "delivery_status": "unknown", "delivery_confirmed": False,
              "message": message}
    if not success or not message_id:
        result["message"] += ". Delivery unverified. Do not resend automatically; the send outcome may be unknown."
        return result
    proof = get_message_delivery_status(recipient, message_id, wait_seconds=15)
    canonical = recipient if "@" in recipient else recipient + "@s.whatsapp.net"
    exact = (proof.get("success") is True and proof.get("message_id") == message_id
             and proof.get("chat_jid") == canonical)
    if exact:
        for field in ("delivery_status", "delivery_confirmed", "receipt_type", "receipt_at", "receipt_observed_at", "receipt_scope"):
            if field in proof:
                result[field] = proof[field]
        # A malformed response must never turn an unknown state into delivery.
        result["delivery_confirmed"] = (proof.get("delivery_confirmed") is True
                                        and proof.get("delivery_status") in ("delivered", "read"))
    if result["delivery_confirmed"]:
        result["message"] = "Message accepted; recipient " + result["delivery_status"] + " receipt verified."
    else:
        result["message"] = "Message accepted, but recipient delivery is not confirmed. Do not resend; use get_message_delivery_status with this chat_jid and message_id."
    return result
