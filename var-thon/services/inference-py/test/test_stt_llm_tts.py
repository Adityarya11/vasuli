"""Manual acceptance check for the full STT -> LLM -> TTS pipeline.

Unlike the VAD tests in this directory, this is not something a script can
grade on its own: it needs a live microphone, live speakers, a running
Ollama instance, and a human to judge whether the transcript was accurate
and the spoken reply sounded right. Run it directly and talk into the mic;
if what comes back sounds like a reasonable reply to what you said, the
pipeline works end to end.

Requires (see docs/setup.md):
    - Ollama running locally with `qwen2.5:3b` pulled
      (`ollama pull qwen2.5:3b`)
    - The Piper voice model at services/inference-py/models/. If it is
      missing, ensure_piper_model() below downloads it there on first run,
      the same file and URL docs/setup.md documents for manual placement.

Run:
    cd services/inference-py
    uv run python test/test_stt_llm_tts.py
"""

import os
import re
import tempfile
import time
import urllib.error
import urllib.request

import ollama
import sounddevice as sd
import speech_recognition as sr
from faster_whisper import WhisperModel
from piper.voice import PiperVoice

TEST_DIR = os.path.dirname(os.path.abspath(__file__))
SERVICE_DIR = os.path.abspath(os.path.join(TEST_DIR, ".."))
MODELS_DIR = os.path.join(SERVICE_DIR, "models")

# Same source docs/setup.md documents for manual download of this model.
PIPER_MODEL_URL = (
    "https://huggingface.co/rhasspy/piper-voices/resolve/main/"
    "en/en_US/lessac/medium/en_US-lessac-medium.onnx"
)
PIPER_CONFIG_URL = PIPER_MODEL_URL + ".json"

PIPER_MODEL_FILE = os.path.join(MODELS_DIR, "en_US-lessac-medium.onnx")
PIPER_CONFIG_FILE = os.path.join(MODELS_DIR, "en_US-lessac-medium.onnx.json")

STT_MODEL_SIZE = "base"
LLM_MODEL = "qwen2.5:3b"
SYSTEM_PROMPT = (
    "You are Ravi, a concise AI voice assistant. "
    "Keep responses under 2 short sentences."
)


def ensure_piper_model() -> None:
    """Downloads the Piper voice model into services/inference-py/models/
    if it isn't already there, so a fresh clone can run this check without
    a manual download step first. Fails loudly and cleans up any partial
    download rather than leaving a truncated model file behind.
    """
    if os.path.exists(PIPER_MODEL_FILE) and os.path.exists(PIPER_CONFIG_FILE):
        return

    os.makedirs(MODELS_DIR, exist_ok=True)
    print(f"[Setup] Piper voice model missing, downloading to {MODELS_DIR}...")

    try:
        urllib.request.urlretrieve(PIPER_MODEL_URL, PIPER_MODEL_FILE)
        urllib.request.urlretrieve(PIPER_CONFIG_URL, PIPER_CONFIG_FILE)
    except (urllib.error.URLError, OSError) as e:
        for partial in (PIPER_MODEL_FILE, PIPER_CONFIG_FILE):
            if os.path.exists(partial):
                os.remove(partial)
        raise RuntimeError(
            f"Failed to download Piper voice model from {PIPER_MODEL_URL}: {e}"
        ) from e

    print("[Setup] Piper voice model downloaded.")


def record_audio(filename: str) -> str:
    recognizer = sr.Recognizer()

    with sr.Microphone() as source:
        print("\n[Microphone] Calibrating for ambient noise...")
        recognizer.adjust_for_ambient_noise(source, duration=1)

        print("[Microphone] Listening...")
        audio = recognizer.listen(source)

    with open(filename, "wb") as f:
        f.write(audio.get_wav_data())

    return filename


def speak_text(text: str, tts_voice: PiperVoice, stream: sd.RawOutputStream) -> None:
    if not text.strip():
        return

    try:
        for chunk in tts_voice.synthesize(text):
            stream.write(chunk.audio_int16_bytes)
    except (RuntimeError, OSError) as e:
        print(f"\n[TTS Error] {e}")


def run_turn(
    stt_model: WhisperModel,
    tts_voice: PiperVoice,
    chat_history: list,
    audio_file: str,
) -> None:
    """Runs one record -> transcribe -> respond -> speak cycle in place,
    appending both turns to chat_history."""

    print("[STT] Transcribing...")
    stt_start = time.time()

    segments, _ = stt_model.transcribe(audio_file, beam_size=5)
    user_text = "".join(segment.text for segment in segments).strip()

    stt_time = time.time() - stt_start
    print(f"\nYou said: '{user_text}' ({stt_time:.2f}s)")

    if not user_text:
        print("[System] No speech detected.")
        return

    chat_history.append({"role": "user", "content": user_text})

    stream = sd.RawOutputStream(
        samplerate=tts_voice.config.sample_rate,
        channels=1,
        dtype="int16",
    )
    stream.start()

    print("AI says: ", end="", flush=True)

    llm_stream = ollama.chat(model=LLM_MODEL, messages=chat_history, stream=True)

    full_response = ""
    text_buffer = ""

    for chunk in llm_stream:
        token = chunk["message"]["content"]
        print(token, end="", flush=True)

        full_response += token
        text_buffer += token

        # Speak each complete sentence as soon as it arrives, instead of
        # waiting for the full LLM response, so TTS overlaps generation.
        while True:
            match = re.search(r"[.!?]", text_buffer)
            if not match:
                break

            end_idx = match.end()
            sentence = text_buffer[:end_idx].strip()
            text_buffer = text_buffer[end_idx:]

            if sentence:
                speak_text(sentence, tts_voice, stream)

    if text_buffer.strip():
        speak_text(text_buffer.strip(), tts_voice, stream)

    print()

    stream.stop()
    stream.close()

    chat_history.append({"role": "assistant", "content": full_response})


def main() -> None:
    ensure_piper_model()

    print("[System] Loading Faster-Whisper...")
    stt_model = WhisperModel(STT_MODEL_SIZE, device="cpu", compute_type="int8")

    print("[System] Loading Piper...")
    tts_voice = PiperVoice.load(PIPER_MODEL_FILE)

    print("[System] Ready.")

    chat_history = [{"role": "system", "content": SYSTEM_PROMPT}]
    audio_file = os.path.join(tempfile.gettempdir(), "var_thon_stt_llm_tts_mic.wav")

    while True:
        try:
            input("\nPress ENTER to record (Ctrl+C to quit)...")
            record_audio(audio_file)
            run_turn(stt_model, tts_voice, chat_history, audio_file)

        except KeyboardInterrupt:
            print("\n\n[System] Exiting...")
            break

        except Exception as e:
            print(f"\n[Error] {e}")

        finally:
            if os.path.exists(audio_file):
                os.remove(audio_file)


if __name__ == "__main__":
    main()
