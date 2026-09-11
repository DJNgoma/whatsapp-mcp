import json
import unittest
from unittest.mock import patch
import outbound_confirmation
import replies

TARGET = {"chat_jid": "99900001@s.whatsapp.net", "message_id": "original",
          "sender_jid": "99900002@lid", "text": "Birthday speech", "fingerprint": "hash"}

class ReplyTests(unittest.TestCase):
    def setUp(self):
        outbound_confirmation._pending.clear()
        self.calls = []
        self.target = dict(TARGET)
        self.mock = patch.object(replies, "_bridge_api_request", side_effect=self.bridge)
        self.mock.start()
        self.addCleanup(self.mock.stop)

    def bridge(self, method, path, **kwargs):
        self.calls.append((method, path, kwargs))
        if method == "GET":
            return {"success": True, "data": dict(self.target)}
        return {"success": True, "message_id": "sent-id", "reply_to_message_id": "original"}

    def preview(self):
        return replies.reply_message(TARGET["chat_jid"], "original", "Happy birthday!")

    def test_preview_then_confirm_exact_payload_and_replay(self):
        p = self.preview()
        self.assertTrue(p["requires_confirmation"])
        self.assertEqual(p["quoted_message"]["sender_jid"], TARGET["sender_jid"])
        self.assertEqual(json.loads(p["preview"])["message"], "Happy birthday!")
        self.assertFalse(any(c[0] == "POST" for c in self.calls))
        r = replies.reply_message(TARGET["chat_jid"], "original", "Happy birthday!", p["confirmation_token"])
        self.assertTrue(r["success"])
        self.assertEqual(self.calls[-1][2]["payload"], {"chat_jid": TARGET["chat_jid"], "message_id": "original", "message": "Happy birthday!", "fingerprint": "hash"})
        r = replies.reply_message(TARGET["chat_jid"], "original", "Happy birthday!", p["confirmation_token"])
        self.assertFalse(r["success"])
        self.assertEqual(sum(c[0] == "POST" for c in self.calls), 1)

    def test_target_changes_invalidate_confirmation(self):
        for field in TARGET:
            with self.subTest(field=field):
                self.target = dict(TARGET)
                p = self.preview()
                self.target[field] = "changed"
                r = replies.reply_message(TARGET["chat_jid"], "original", "Happy birthday!", p["confirmation_token"])
                self.assertFalse(r["success"])
        self.assertFalse(any(c[0] == "POST" for c in self.calls))

    def test_changed_reply_rejected(self):
        p = self.preview()
        r = replies.reply_message(TARGET["chat_jid"], "original", "Different", p["confirmation_token"])
        self.assertFalse(r["success"])
        self.assertFalse(any(c[0] == "POST" for c in self.calls))

    def test_preview_failure_does_not_send(self):
        with patch.object(replies, "_bridge_api_request", return_value={"success": False, "message": "Not found"}) as api:
            self.assertFalse(self.preview()["success"])
            self.assertEqual(api.call_count, 1)
        self.assertFalse(replies.reply_message("", "original", "text")["success"])

    def test_mcp_tool_is_registered_with_confirmation_parameter(self):
        import asyncio
        import main
        tools = asyncio.run(main.mcp.list_tools())
        tool = next(t for t in tools if t.name == "reply_message")
        self.assertIn("confirmation_token", tool.inputSchema["properties"])
        self.assertEqual(set(tool.inputSchema["required"]), {"chat_jid", "message_id", "message"})

    def test_text_message_output_includes_quote_identifiers(self):
        from datetime import datetime
        import whatsapp
        msg = whatsapp.Message(timestamp=datetime.now(), sender="self", content="text", is_from_me=True,
                               chat_jid=TARGET["chat_jid"], id="original", chat_name="Self", media_type="")
        output = whatsapp.format_message(msg)
        self.assertIn("Message ID: original", output)
        self.assertIn("Chat JID: " + TARGET["chat_jid"], output)
