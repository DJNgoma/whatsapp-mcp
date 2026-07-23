import json
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from contextlib import closing, redirect_stdout
from io import StringIO
from pathlib import Path
from unittest import mock

import outbound_confirmation
import main
import macos_contacts
import whatsapp


class RecordingCursor:
    def __init__(self, row=None):
        self.row = row
        self.calls = []

    def execute(self, statement, parameters=()):
        self.calls.append((statement, tuple(parameters)))
        return self

    def fetchone(self):
        return self.row

    def fetchall(self):
        return []


class RecordingConnection:
    def __init__(self, cursor):
        self._cursor = cursor

    def cursor(self):
        return self._cursor

    def close(self):
        pass


class OutboundConfirmationTests(unittest.TestCase):
    def setUp(self):
        outbound_confirmation._pending.clear()

    def test_confirmation_is_exact_and_one_time(self):
        draft = outbound_confirmation.prepare("text", "27820000000", "Hello")
        token = draft["confirmation_token"]

        confirmed, _ = outbound_confirmation.consume(
            token, "text", "27820000000", "Changed"
        )
        self.assertFalse(confirmed)

        draft = outbound_confirmation.prepare("text", "27820000000", "Hello")
        token = draft["confirmation_token"]
        confirmed, _ = outbound_confirmation.consume(
            token, "text", "27820000000", "Hello"
        )
        self.assertTrue(confirmed)

        confirmed_again, _ = outbound_confirmation.consume(
            token, "text", "27820000000", "Hello"
        )
        self.assertFalse(confirmed_again)

    def test_first_send_tool_call_only_prepares(self):
        with mock.patch.object(
            main, "whatsapp_send_message", side_effect=AssertionError("must not send")
        ):
            result = main.send_message("27820000000", "Preview only")

        self.assertFalse(result["success"])
        self.assertTrue(result["requires_confirmation"])
        self.assertEqual(result["preview"], "Preview only")

    def test_group_and_chat_mutations_only_prepare_on_first_call(self):
        with mock.patch.object(
            main, "whatsapp_create_group", side_effect=AssertionError("must not create")
        ), mock.patch.object(
            main,
            "whatsapp_set_chat_read_state",
            side_effect=AssertionError("must not update"),
        ):
            group_result = main.create_group("Field Team", ["+27820000000"])
            read_result = main.set_chat_read_state(
                "27820000000@s.whatsapp.net", True
            )

        self.assertTrue(group_result["requires_confirmation"])
        self.assertIn("Field Team", group_result["preview"])
        self.assertTrue(read_result["requires_confirmation"])
        self.assertIn('"read":true', read_result["preview"])

    def test_requested_capability_tools_are_registered(self):
        expected = {
            "get_own_identity",
            "search_contacts_chats_and_groups",
            "list_joined_groups",
            "get_group_info",
            "create_group",
            "add_participants_to_group",
            "remove_participants_from_group",
            "promote_participants_to_admins",
            "demote_participants_from_admins",
            "set_chat_read_state",
            "is_on_whatsapp",
            "set_chat_mute",
            "set_chat_archive",
            "set_contact_block",
            "send_typing_indicator",
            "whisper_status",
            "transcribe_audio_message",
        }
        self.assertTrue(expected.issubset(main.mcp._tool_manager._tools.keys()))

    def test_audio_transcription_downloads_then_uses_local_whisper(self):
        transcript = {"success": True, "text": "A short test message."}
        with mock.patch.object(
            main, "whatsapp_download_media", return_value="/tmp/test-message.ogg"
        ) as download, mock.patch.object(
            main, "whisper_transcribe_audio_file", return_value=transcript.copy()
        ) as transcribe:
            result = main.transcribe_audio_message(
                "message-123", "15551234567@s.whatsapp.net", "en"
            )

        download.assert_called_once_with(
            "message-123", "15551234567@s.whatsapp.net"
        )
        transcribe.assert_called_once_with("/tmp/test-message.ogg", "en", None)
        self.assertTrue(result["success"])
        self.assertEqual(result["text"], "A short test message.")


class PaginationBoundaryTests(unittest.TestCase):
    def test_mcp_tools_clamp_negative_and_huge_pagination_values(self):
        huge = 10**100
        with mock.patch.object(
            main, "whatsapp_list_messages", return_value=[]
        ) as messages:
            main.list_messages(
                limit=-5,
                page=huge,
                context_before=-2,
                context_after=huge,
            )
        self.assertEqual(messages.call_args.kwargs["limit"], 1)
        self.assertEqual(messages.call_args.kwargs["page"], main.MAX_PAGE_NUMBER)
        self.assertEqual(messages.call_args.kwargs["context_before"], 0)
        self.assertEqual(
            messages.call_args.kwargs["context_after"], main.MAX_CONTEXT_MESSAGES
        )

        with mock.patch.object(main, "whatsapp_list_chats", return_value=[]) as chats:
            main.list_chats(limit=huge, page=-1)
        self.assertEqual(chats.call_args.kwargs["limit"], main.MAX_PAGE_SIZE)
        self.assertEqual(chats.call_args.kwargs["page"], 0)

        with mock.patch.object(
            main, "whatsapp_get_contact_chats", return_value=[]
        ) as contact_chats:
            main.get_contact_chats("contact@s.whatsapp.net", limit=-1, page=huge)
        contact_chats.assert_called_once_with(
            "contact@s.whatsapp.net", 1, main.MAX_PAGE_NUMBER
        )

        with mock.patch.object(
            main, "whatsapp_get_message_context", return_value={}
        ) as context:
            main.get_message_context("message-id", before=-1, after=huge)
        context.assert_called_once_with(
            "message-id", 0, main.MAX_CONTEXT_MESSAGES
        )

    def test_database_helpers_reapply_pagination_bounds(self):
        huge = 10**100

        message_cursor = RecordingCursor()
        with mock.patch.object(
            whatsapp.sqlite3,
            "connect",
            return_value=RecordingConnection(message_cursor),
        ):
            whatsapp.list_messages(
                limit=-1, page=huge, include_context=False
            )
        self.assertEqual(
            message_cursor.calls[-1][1][-2:],
            (1, whatsapp.MAX_PAGE_NUMBER),
        )

        chat_cursor = RecordingCursor()
        with mock.patch.object(
            whatsapp.sqlite3,
            "connect",
            return_value=RecordingConnection(chat_cursor),
        ):
            whatsapp.list_chats(limit=huge, page=-1)
        self.assertEqual(
            chat_cursor.calls[-1][1][-2:],
            (whatsapp.MAX_PAGE_SIZE, 0),
        )

        contact_cursor = RecordingCursor()
        with mock.patch.object(
            whatsapp.sqlite3,
            "connect",
            return_value=RecordingConnection(contact_cursor),
        ):
            whatsapp.get_contact_chats(
                "contact@s.whatsapp.net", limit=-1, page=huge
            )
        self.assertEqual(
            contact_cursor.calls[-1][1][-2:],
            (1, whatsapp.MAX_PAGE_NUMBER),
        )

        target = (
            "2026-01-01T00:00:00+00:00",
            "sender@s.whatsapp.net",
            "Test chat",
            "Target",
            False,
            "chat@s.whatsapp.net",
            "message-id",
            "chat@s.whatsapp.net",
            None,
        )
        context_cursor = RecordingCursor(row=target)
        with mock.patch.object(
            whatsapp.sqlite3,
            "connect",
            return_value=RecordingConnection(context_cursor),
        ):
            whatsapp.get_message_context("message-id", before=-1, after=huge)
        self.assertEqual(context_cursor.calls[1][1][-1], 0)
        self.assertEqual(
            context_cursor.calls[2][1][-1], whatsapp.MAX_CONTEXT_MESSAGES
        )


class ContactSearchTests(unittest.TestCase):
    BLANK_QUERIES = ("", " ", "\t", "\n", " \t\n ")

    def test_mcp_contact_search_rejects_blank_queries(self):
        with mock.patch.object(main, "whatsapp_search_contacts") as search_contacts:
            for query in self.BLANK_QUERIES:
                with self.subTest(query=repr(query)):
                    self.assertEqual(main.search_contacts(query), [])

        search_contacts.assert_not_called()

    def test_mcp_contact_search_preserves_positive_query(self):
        contacts = [{"name": "Known Person", "phone_number": "27820000000"}]
        with mock.patch.object(
            main, "whatsapp_search_contacts", return_value=contacts
        ) as search_contacts:
            self.assertIs(main.search_contacts("Known Person"), contacts)

        search_contacts.assert_called_once_with("Known Person")

    def test_mcp_unified_contact_search_rejects_blank_queries(self):
        with mock.patch.object(
            main, "whatsapp_search_contacts_chats_and_groups"
        ) as search_contacts:
            for query in self.BLANK_QUERIES:
                with self.subTest(query=repr(query)):
                    self.assertEqual(
                        main.search_contacts_chats_and_groups(query, limit=10),
                        [],
                    )

        search_contacts.assert_not_called()

    def test_mcp_unified_contact_search_preserves_phone_fragment(self):
        contacts = [{"type": "direct_chat", "phone_number": "27820000000"}]
        with mock.patch.object(
            main,
            "whatsapp_search_contacts_chats_and_groups",
            return_value=contacts,
        ) as search_contacts:
            self.assertIs(
                main.search_contacts_chats_and_groups("2782", limit=7),
                contacts,
            )

        search_contacts.assert_called_once_with("2782", 7)

    def test_macos_contacts_wrapper_rejects_blank_queries(self):
        with mock.patch.object(macos_contacts, "_run_contacts_helper") as helper:
            for query in self.BLANK_QUERIES:
                with self.subTest(query=repr(query)):
                    self.assertEqual(macos_contacts.search_macos_contacts(query), ([], None))

        helper.assert_not_called()

    def test_macos_contacts_wrapper_preserves_positive_query(self):
        with mock.patch.object(
            macos_contacts.platform, "system", return_value="Darwin"
        ), mock.patch.object(
            macos_contacts,
            "HELPER_PATH",
            mock.Mock(is_file=mock.Mock(return_value=True)),
        ), mock.patch.object(
            macos_contacts, "_run_contacts_helper", return_value=[]
        ) as helper:
            self.assertEqual(
                macos_contacts.search_macos_contacts("Known Person", limit=7),
                ([], None),
            )

        helper.assert_called_once_with("Known Person", "7")

    def test_unified_empty_query_does_not_invoke_native_contacts_helper(self):
        with mock.patch.object(
            whatsapp, "list_chats", return_value=[]
        ), mock.patch.object(
            whatsapp, "_search_whatsapp_contacts", return_value=[]
        ), mock.patch.object(
            macos_contacts, "_run_contacts_helper"
        ) as helper:
            self.assertEqual(
                whatsapp.search_contacts_chats_and_groups("", limit=10),
                [],
            )

        helper.assert_not_called()

    @unittest.skipUnless(
        sys.platform == "darwin" and shutil.which("xcrun"),
        "requires the macOS Swift compiler",
    )
    def test_native_contacts_helper_rejects_blank_queries_before_contacts_access(self):
        helper_source = (
            Path(__file__).parents[1] / "macos-contacts-helper" / "main.swift"
        )
        with tempfile.TemporaryDirectory() as temporary_directory:
            helper_binary = Path(temporary_directory) / "whatsapp-mcp-contacts"
            subprocess.run(
                ["xcrun", "swiftc", str(helper_source), "-o", str(helper_binary)],
                check=True,
                capture_output=True,
                text=True,
            )
            for query in self.BLANK_QUERIES:
                with self.subTest(query=repr(query)):
                    completed = subprocess.run(
                        [str(helper_binary), query],
                        check=True,
                        capture_output=True,
                        text=True,
                        timeout=5,
                    )
                    self.assertEqual(json.loads(completed.stdout), [])
                    self.assertEqual(completed.stderr, "")

    def test_unavailable_macos_contacts_are_silent_on_other_platforms(self):
        with mock.patch.object(whatsapp, "_search_whatsapp_contacts", return_value=[]), mock.patch.object(
            whatsapp,
            "search_macos_contacts",
            return_value=([], "Contacts.app lookup is available only on macOS."),
        ):
            output = StringIO()
            with redirect_stdout(output):
                self.assertEqual(whatsapp.search_contacts("person"), [])

        self.assertEqual(output.getvalue(), "")

    def test_macos_contacts_are_merged_with_known_whatsapp_chats(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            database_path = Path(temporary_directory) / "messages.db"
            with closing(sqlite3.connect(database_path)) as connection:
                connection.execute(
                    "CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)"
                )
                connection.execute(
                    "INSERT INTO chats VALUES (?, ?, ?)",
                    ("27820000000@s.whatsapp.net", None, None),
                )
                connection.commit()

            contacts = [
                {"name": "Known Person", "phone_number": "+27 82 000 0000"},
                {"name": "Address Book Only", "phone_number": "+27 83 000 0000"},
            ]
            with mock.patch.object(whatsapp, "MESSAGES_DB_PATH", str(database_path)), mock.patch.object(
                whatsapp, "search_macos_contacts", return_value=(contacts, None)
            ):
                result = whatsapp.search_contacts("278")

        by_phone = {contact.phone_number: contact for contact in result}
        self.assertEqual(by_phone["27820000000"].name, "Known Person")
        self.assertTrue(by_phone["27820000000"].has_whatsapp_chat)
        self.assertEqual(by_phone["27820000000"].source, "whatsapp+macos_contacts")
        self.assertFalse(by_phone["27830000000"].has_whatsapp_chat)
        self.assertIsNone(by_phone["27830000000"].jid)

    def test_unified_search_includes_direct_chats_groups_and_address_book_contacts(self):
        chats = [
            whatsapp.Chat(
                jid="27820000000@s.whatsapp.net",
                name="Known Person",
                last_message_time=None,
            ),
            whatsapp.Chat(
                jid="120363000000000000@g.us",
                name="Field Team",
                last_message_time=None,
            ),
        ]
        contacts = [
            whatsapp.Contact(
                phone_number="27820000000",
                name="Known Person",
                jid="27820000000@s.whatsapp.net",
            ),
            whatsapp.Contact(
                phone_number="27830000000",
                name="Address Book Only",
                jid=None,
                source="macos_contacts",
                has_whatsapp_chat=False,
            ),
        ]
        with mock.patch.object(whatsapp, "list_chats", return_value=chats), mock.patch.object(
            whatsapp, "search_contacts", return_value=contacts
        ):
            result = whatsapp.search_contacts_chats_and_groups("27", limit=10)

        self.assertEqual(
            [item["type"] for item in result],
            ["direct_chat", "group", "contact"],
        )


if __name__ == "__main__":
    unittest.main()
