"""Optional, local Whisper transcription backed by faster-whisper."""

import importlib.util
import os
from functools import lru_cache
from pathlib import Path
from typing import Any, Dict, Optional


DEFAULT_MODEL = "base"
DEFAULT_MAX_FILE_BYTES = 25 * 1024 * 1024
DEFAULT_MAX_DURATION_SECONDS = 15 * 60
DEFAULT_MAX_SEGMENTS = 500
DEFAULT_MAX_OUTPUT_CHARS = 100_000


class AudioDurationLimitError(ValueError):
    pass


def _positive_int_setting(name: str, default: int) -> int:
    try:
        value = int(os.environ.get(name, str(default)))
    except (TypeError, ValueError, OverflowError):
        return default
    return value if value > 0 else default


def _settings() -> Dict[str, Any]:
    return {
        "model": os.environ.get("WHATSAPP_WHISPER_MODEL", DEFAULT_MODEL),
        "device": os.environ.get("WHATSAPP_WHISPER_DEVICE", "auto"),
        "compute_type": os.environ.get("WHATSAPP_WHISPER_COMPUTE_TYPE", "default"),
        "max_file_bytes": _positive_int_setting(
            "WHATSAPP_WHISPER_MAX_FILE_BYTES", DEFAULT_MAX_FILE_BYTES
        ),
        "max_duration_seconds": _positive_int_setting(
            "WHATSAPP_WHISPER_MAX_DURATION_SECONDS", DEFAULT_MAX_DURATION_SECONDS
        ),
        "max_segments": _positive_int_setting(
            "WHATSAPP_WHISPER_MAX_SEGMENTS", DEFAULT_MAX_SEGMENTS
        ),
        "max_output_chars": _positive_int_setting(
            "WHATSAPP_WHISPER_MAX_OUTPUT_CHARS", DEFAULT_MAX_OUTPUT_CHARS
        ),
    }


def whisper_status() -> Dict[str, Any]:
    settings = _settings()
    installed = importlib.util.find_spec("faster_whisper") is not None
    return {
        "available": installed,
        "backend": "faster-whisper",
        "model": settings["model"],
        "device": settings["device"],
        "compute_type": settings["compute_type"],
        "limits": {
            "max_file_bytes": settings["max_file_bytes"],
            "max_duration_seconds": settings["max_duration_seconds"],
            "max_segments": settings["max_segments"],
            "max_output_chars": settings["max_output_chars"],
        },
        "local_processing": True,
        "model_download_required_on_first_use": True,
        "message": (
            "Local Whisper transcription is available."
            if installed
            else "Whisper is optional. Install it with: uv sync --extra whisper"
        ),
    }


@lru_cache(maxsize=4)
def _load_model(model_name: str, device: str, compute_type: str):
    try:
        from faster_whisper import WhisperModel
    except ImportError as error:
        raise RuntimeError(
            "Whisper support is not installed. Run: uv sync --extra whisper"
        ) from error

    return WhisperModel(model_name, device=device, compute_type=compute_type)


def _decoded_audio_duration(path: Path, max_duration_seconds: int) -> float:
    """Decode frames without retaining them and enforce an actual duration cap."""
    try:
        import av
    except ImportError as error:
        raise RuntimeError(
            "Whisper support is incomplete because PyAV is not installed. "
            "Run: uv sync --extra whisper"
        ) from error

    duration = 0.0
    try:
        with av.open(str(path), mode="r", metadata_errors="ignore") as container:
            if not container.streams.audio:
                raise ValueError("file has no audio stream")
            for frame in container.decode(audio=0):
                sample_rate = int(frame.sample_rate or 0)
                if sample_rate <= 0:
                    raise ValueError("audio frame has no valid sample rate")
                duration += float(frame.samples) / sample_rate
                if duration > max_duration_seconds:
                    raise AudioDurationLimitError(
                        f"Audio exceeds the {max_duration_seconds}-second transcription limit"
                    )
    except AudioDurationLimitError:
        raise
    except Exception as error:
        raise ValueError(f"could not inspect audio safely: {error}") from error
    return duration


def transcribe_audio_file(
    audio_path: str,
    language: Optional[str] = None,
    initial_prompt: Optional[str] = None,
) -> Dict[str, Any]:
    path = Path(audio_path).expanduser()
    if not path.is_file():
        return {"success": False, "message": f"Audio file not found: {path}"}

    settings = _settings()
    try:
        file_size = path.stat().st_size
        if file_size > settings["max_file_bytes"]:
            return {
                "success": False,
                "message": (
                    "Audio exceeds the configured transcription file-size limit "
                    f"({file_size} > {settings['max_file_bytes']} bytes)"
                ),
            }
        model = _load_model(
            settings["model"], settings["device"], settings["compute_type"]
        )
        duration = _decoded_audio_duration(path, settings["max_duration_seconds"])
        if duration > settings["max_duration_seconds"]:
            raise AudioDurationLimitError(
                "Audio exceeds the configured transcription duration limit"
            )
        segments, info = model.transcribe(
            str(path),
            language=language or None,
            initial_prompt=initial_prompt or None,
            beam_size=5,
            vad_filter=True,
            clip_timestamps=f"0,{settings['max_duration_seconds']}",
        )
        rendered_segments = []
        rendered_characters = 0
        for segment in segments:
            text = segment.text.strip()
            if not text:
                continue
            if len(rendered_segments) >= settings["max_segments"]:
                return {
                    "success": False,
                    "message": "Whisper transcription exceeded the segment limit",
                }
            next_character_count = rendered_characters + len(text)
            if rendered_segments:
                next_character_count += 1
            if next_character_count > settings["max_output_chars"]:
                return {
                    "success": False,
                    "message": "Whisper transcription exceeded the output-size limit",
                }
            rendered_segments.append({
                "start": round(float(segment.start), 3),
                "end": round(float(segment.end), 3),
                "text": text,
            })
            rendered_characters = next_character_count
    except Exception as error:
        return {"success": False, "message": f"Whisper transcription failed: {error}"}

    text = " ".join(segment["text"] for segment in rendered_segments).strip()
    return {
        "success": True,
        "text": text,
        "language": getattr(info, "language", language),
        "language_probability": getattr(info, "language_probability", None),
        "duration_seconds": getattr(info, "duration", duration),
        "segments": rendered_segments,
        "model": settings["model"],
        "device": settings["device"],
        "file_path": str(path.resolve()),
        "local_processing": True,
    }
