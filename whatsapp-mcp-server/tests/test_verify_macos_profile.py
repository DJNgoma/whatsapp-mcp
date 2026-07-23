import importlib.util
import unittest
from pathlib import Path
from unittest import mock


SCRIPT_PATH = Path(__file__).resolve().parents[2] / "scripts" / "verify_macos_profile.py"
SPEC = importlib.util.spec_from_file_location("verify_macos_profile", SCRIPT_PATH)
verifier = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(verifier)


class ProfileInstanceTests(unittest.TestCase):
    def test_verify_rejects_health_from_another_instance(self):
        label = verifier.profile_label("company")
        plist = {
            "ProgramArguments": [
                "/synthetic/bridge",
                "--port",
                "8742",
                "--store-dir",
                "/synthetic/store",
            ]
        }
        with mock.patch.object(verifier, "LAUNCH_AGENTS_DIR") as launch_agents, mock.patch(
            "pathlib.Path.exists",
            return_value=True,
        ), mock.patch("pathlib.Path.open", mock.mock_open()), mock.patch.object(
            verifier.plistlib,
            "load",
            return_value=plist,
        ), mock.patch.object(
            verifier.subprocess,
            "run",
        ) as launchctl, mock.patch.object(
            verifier.os,
            "getuid",
            create=True,
            return_value=501,
        ), mock.patch.object(
            verifier,
            "fetch_json",
            return_value={"instance_id": "another-instance"},
        ):
            launch_agents.__truediv__.return_value = Path(f"/synthetic/{label}.plist")
            launchctl.return_value.returncode = 0
            with self.assertRaisesRegex(RuntimeError, "different bridge instance"):
                verifier.verify("company", 8742, "+15551234567")

    def test_profile_label_is_the_expected_instance_id(self):
        self.assertEqual(
            verifier.profile_label("company"),
            "com.djngoma.whatsapp-mcp-bridge.company",
        )


if __name__ == "__main__":
    unittest.main()
