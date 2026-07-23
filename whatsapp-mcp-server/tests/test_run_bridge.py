import contextlib
import importlib.util
import io
import os
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock


SCRIPT_PATH = Path(__file__).resolve().parents[2] / "scripts" / "run_bridge.py"
SPEC = importlib.util.spec_from_file_location("run_bridge", SCRIPT_PATH)
run_bridge = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(run_bridge)


class DefaultStateDirTests(unittest.TestCase):
    def test_macos_state_dir(self):
        self.assertEqual(
            run_bridge.default_state_dir(platform="darwin", environment={}, home=Path("/Users/test")),
            Path("/Users/test/Library/Application Support/whatsapp-mcp"),
        )

    def test_linux_honors_xdg_data_home(self):
        self.assertEqual(
            run_bridge.default_state_dir(
                platform="linux", environment={"XDG_DATA_HOME": "/data"}, home=Path("/home/test")
            ),
            Path("/data/whatsapp-mcp"),
        )

    def test_windows_uses_local_app_data(self):
        self.assertEqual(
            run_bridge.default_state_dir(
                platform="win32",
                environment={"LOCALAPPDATA": "C:/Users/test/AppData/Local"},
                home=Path("C:/Users/test"),
            ),
            Path("C:/Users/test/AppData/Local") / "whatsapp-mcp",
        )


class StorePermissionTests(unittest.TestCase):
    @unittest.skipIf(os.name == "nt", "POSIX mode assertions do not apply on Windows")
    def test_existing_unix_store_is_tightened(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            store_dir = Path(temporary_directory) / "store"
            store_dir.mkdir(mode=0o755)
            store_dir.chmod(0o755)

            run_bridge.ensure_private_store_directory(store_dir, platform="linux")

            self.assertEqual(stat.S_IMODE(store_dir.stat().st_mode), 0o700)

    def test_windows_store_uses_acl_without_posix_chmod(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            store_dir = Path(temporary_directory) / "store"
            with mock.patch.object(Path, "chmod") as chmod:
                run_bridge.ensure_private_store_directory(store_dir, platform="win32")

            self.assertTrue(store_dir.is_dir())
            chmod.assert_not_called()


class InstanceIdTests(unittest.TestCase):
    def test_instance_id_validation(self):
        self.assertEqual(run_bridge.normalize_instance_id("work-1"), "work-1")
        with self.assertRaises(ValueError):
            run_bridge.normalize_instance_id("work profile")

    def test_main_passes_instance_id_to_bridge_environment(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            store_dir = Path(temporary_directory) / "store"
            arguments = [
                "run_bridge.py",
                "--store-dir",
                str(store_dir),
                "--instance-id",
                "work-bridge",
            ]
            with mock.patch.object(sys, "argv", arguments), mock.patch.object(
                run_bridge.subprocess,
                "run",
                return_value=SimpleNamespace(returncode=0),
            ) as subprocess_run, contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(run_bridge.main(), 0)

            child_environment = subprocess_run.call_args.kwargs["env"]
            self.assertEqual(
                child_environment["WHATSAPP_BRIDGE_INSTANCE_ID"],
                "work-bridge",
            )
            if os.name != "nt":
                self.assertEqual(stat.S_IMODE(store_dir.stat().st_mode), 0o700)


if __name__ == "__main__":
    unittest.main()
