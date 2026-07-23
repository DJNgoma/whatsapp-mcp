import importlib.util
import io
import json
import os
import plistlib
import stat
import tempfile
import unittest
from pathlib import Path
from unittest import mock


SCRIPT_PATH = (
    Path(__file__).resolve().parents[2] / "scripts" / "install_macos_launch_agent.py"
)
SPEC = importlib.util.spec_from_file_location("install_macos_launch_agent", SCRIPT_PATH)
installer = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(installer)


def permissions(path: Path) -> int:
    return stat.S_IMODE(path.stat().st_mode)


@unittest.skipIf(os.name == "nt", "POSIX mode assertions do not apply on Windows")
class StorePermissionTests(unittest.TestCase):
    def test_existing_store_and_files_are_tightened(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            store_dir = Path(temporary_directory) / "store"
            store_dir.mkdir(mode=0o755)
            database = store_dir / "messages.db"
            database.write_bytes(b"not a real database")
            database.chmod(0o644)

            installer.migrate_store(store_dir, "company")

            self.assertEqual(permissions(store_dir), 0o700)
            self.assertEqual(permissions(database), 0o600)

    def test_copied_sensitive_tree_is_private(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_root = Path(temporary_directory)
            source_store = temporary_root / "source"
            source_media = source_store / "media" / "chat"
            source_media.mkdir(parents=True)
            source_file = source_media / "voice-note.ogg"
            source_file.write_bytes(b"synthetic")
            source_file.chmod(0o644)
            destination = temporary_root / "destination"

            with mock.patch.object(installer, "SOURCE_STORE", source_store):
                installer.migrate_store(destination, installer.DEFAULT_PROFILE)

            copied_media = destination / "media"
            copied_chat = copied_media / "chat"
            copied_file = copied_chat / source_file.name
            self.assertEqual(permissions(destination), 0o700)
            self.assertEqual(permissions(copied_media), 0o700)
            self.assertEqual(permissions(copied_chat), 0o700)
            self.assertEqual(permissions(copied_file), 0o600)


class LaunchAgentTests(unittest.TestCase):
    @unittest.skipIf(os.name == "nt", "POSIX mode assertions do not apply on Windows")
    def test_plist_binds_instance_and_hardens_existing_logs(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_root = Path(temporary_directory)
            launch_agents = temporary_root / "LaunchAgents"
            log_dir = temporary_root / "logs"
            log_dir.mkdir(mode=0o755)
            log_file = log_dir / "bridge.log"
            log_file.write_text("synthetic", encoding="utf-8")
            log_file.chmod(0o644)
            binary_path = temporary_root / "bin" / "bridge"
            store_dir = temporary_root / "store"

            with mock.patch.object(installer, "LAUNCH_AGENTS_DIR", launch_agents), mock.patch.object(
                installer,
                "run",
            ):
                installer.write_launch_agent(
                    binary_path,
                    store_dir,
                    log_dir,
                    8742,
                    "company",
                )

            plist_path = launch_agents / f"{installer.profile_label('company')}.plist"
            with plist_path.open("rb") as plist_file:
                configuration = plistlib.load(plist_file)

            self.assertEqual(permissions(log_dir), 0o700)
            self.assertEqual(permissions(log_file), 0o600)
            self.assertEqual(permissions(plist_path), 0o600)
            self.assertEqual(
                configuration["EnvironmentVariables"]["WHATSAPP_BRIDGE_INSTANCE_ID"],
                installer.profile_label("company"),
            )

    def test_health_wait_rejects_wrong_instance_before_accepting_expected(self):
        wrong = io.BytesIO(
            json.dumps(
                {
                    "instance_id": "another-instance",
                    "connected": True,
                    "logged_in": True,
                }
            ).encode("utf-8")
        )
        expected = io.BytesIO(
            json.dumps(
                {
                    "instance_id": "expected-instance",
                    "connected": True,
                    "logged_in": True,
                }
            ).encode("utf-8")
        )
        with mock.patch.object(
            installer.urllib.request,
            "urlopen",
            side_effect=[wrong, expected],
        ) as urlopen, mock.patch.object(installer.time, "sleep"):
            health = installer.wait_for_health(
                8741,
                "expected-instance",
                timeout=1,
            )

        self.assertEqual(health["instance_id"], "expected-instance")
        self.assertEqual(urlopen.call_count, 2)


if __name__ == "__main__":
    unittest.main()
