import os
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import transcription


class FakeModel:
    def transcribe(self, path, **options):
        self.path = path
        self.options = options
        segments = [
            SimpleNamespace(start=0.0, end=1.25, text=" Hello "),
            SimpleNamespace(start=1.25, end=2.5, text="from Whisper."),
        ]
        info = SimpleNamespace(
            language="en", language_probability=0.99, duration=2.5
        )
        return iter(segments), info


class WhisperTranscriptionTests(unittest.TestCase):
    def setUp(self):
        transcription._load_model.cache_clear()

    def test_missing_optional_dependency_is_reported(self):
        with mock.patch.object(
            transcription.importlib.util, "find_spec", return_value=None
        ):
            status = transcription.whisper_status()

        self.assertFalse(status["available"])
        self.assertIn("uv sync --extra whisper", status["message"])

    def test_status_reports_configured_resource_limits(self):
        configured = {
            "WHATSAPP_WHISPER_MAX_FILE_BYTES": "1024",
            "WHATSAPP_WHISPER_MAX_DURATION_SECONDS": "30",
            "WHATSAPP_WHISPER_MAX_SEGMENTS": "12",
            "WHATSAPP_WHISPER_MAX_OUTPUT_CHARS": "2048",
        }
        with mock.patch.dict(os.environ, configured):
            limits = transcription.whisper_status()["limits"]

        self.assertEqual(limits["max_file_bytes"], 1024)
        self.assertEqual(limits["max_duration_seconds"], 30)
        self.assertEqual(limits["max_segments"], 12)
        self.assertEqual(limits["max_output_chars"], 2048)

    def test_transcription_is_local_and_returns_segments(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            audio_path = Path(temporary_directory) / "message.ogg"
            audio_path.write_bytes(b"not-real-audio")
            model = FakeModel()
            with mock.patch.object(
                transcription, "_load_model", return_value=model
            ), mock.patch.object(
                transcription, "_decoded_audio_duration", return_value=2.5
            ):
                result = transcription.transcribe_audio_file(
                    str(audio_path), language="en", initial_prompt="WhatsApp MCP"
                )

        self.assertTrue(result["success"])
        self.assertEqual(result["text"], "Hello from Whisper.")
        self.assertEqual(result["language"], "en")
        self.assertTrue(result["local_processing"])
        self.assertEqual(len(result["segments"]), 2)
        self.assertEqual(model.options["language"], "en")
        self.assertEqual(model.options["initial_prompt"], "WhatsApp MCP")
        self.assertEqual(
            model.options["clip_timestamps"],
            f"0,{transcription.DEFAULT_MAX_DURATION_SECONDS}",
        )

    def test_missing_backend_error_is_preserved_for_transcription(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            audio_path = Path(temporary_directory) / "message.ogg"
            audio_path.write_bytes(b"short")
            with mock.patch.object(
                transcription,
                "_load_model",
                side_effect=RuntimeError(
                    "Whisper support is not installed. Run: uv sync --extra whisper"
                ),
            ), mock.patch.object(
                transcription,
                "_decoded_audio_duration",
                side_effect=AssertionError("duration probe must not run"),
            ):
                result = transcription.transcribe_audio_file(str(audio_path))

        self.assertFalse(result["success"])
        self.assertIn("Whisper support is not installed", result["message"])

    def test_file_size_limit_is_configurable_and_checked_before_model_load(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            audio_path = Path(temporary_directory) / "message.ogg"
            audio_path.write_bytes(b"12345")
            with mock.patch.dict(
                os.environ, {"WHATSAPP_WHISPER_MAX_FILE_BYTES": "4"}
            ), mock.patch.object(
                transcription,
                "_load_model",
                side_effect=AssertionError("oversized files must be rejected first"),
            ):
                result = transcription.transcribe_audio_file(str(audio_path))

        self.assertFalse(result["success"])
        self.assertIn("file-size limit", result["message"])

    def test_duration_limit_rejects_audio_before_inference(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            audio_path = Path(temporary_directory) / "message.ogg"
            audio_path.write_bytes(b"short")
            model = FakeModel()
            with mock.patch.dict(
                os.environ, {"WHATSAPP_WHISPER_MAX_DURATION_SECONDS": "2"}
            ), mock.patch.object(
                transcription, "_load_model", return_value=model
            ), mock.patch.object(
                transcription, "_decoded_audio_duration", return_value=2.1
            ):
                result = transcription.transcribe_audio_file(str(audio_path))

        self.assertFalse(result["success"])
        self.assertIn("duration limit", result["message"])
        self.assertFalse(hasattr(model, "path"))

    def test_segment_limit_stops_unbounded_result_materialization(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            audio_path = Path(temporary_directory) / "message.ogg"
            audio_path.write_bytes(b"short")
            with mock.patch.dict(
                os.environ, {"WHATSAPP_WHISPER_MAX_SEGMENTS": "1"}
            ), mock.patch.object(
                transcription, "_load_model", return_value=FakeModel()
            ), mock.patch.object(
                transcription, "_decoded_audio_duration", return_value=2.5
            ):
                result = transcription.transcribe_audio_file(str(audio_path))

        self.assertFalse(result["success"])
        self.assertIn("segment limit", result["message"])

    def test_output_limit_stops_unbounded_transcript_text(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            audio_path = Path(temporary_directory) / "message.ogg"
            audio_path.write_bytes(b"short")
            with mock.patch.dict(
                os.environ, {"WHATSAPP_WHISPER_MAX_OUTPUT_CHARS": "10"}
            ), mock.patch.object(
                transcription, "_load_model", return_value=FakeModel()
            ), mock.patch.object(
                transcription, "_decoded_audio_duration", return_value=2.5
            ):
                result = transcription.transcribe_audio_file(str(audio_path))

        self.assertFalse(result["success"])
        self.assertIn("output-size limit", result["message"])


if __name__ == "__main__":
    unittest.main()
