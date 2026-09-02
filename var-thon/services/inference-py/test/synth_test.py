"""Synthetic-pause check: splices two real speech clips together with an
injected digital silence gap shorter than min_silence_duration, to confirm
VADDetector treats it as a mid-utterance pause rather than an utterance
boundary. Complements test_vad.py, which uses a single unmodified
recording instead of a spliced one.
"""

import os
import sys

import numpy as np
import scipy.signal
import wave

TEST_DIR = os.path.dirname(__file__)
SERVICE_DIR = os.path.abspath(os.path.join(TEST_DIR, ".."))
REPO_ROOT = os.path.abspath(os.path.join(SERVICE_DIR, "..", ".."))

sys.path.insert(0, SERVICE_DIR)

from vad.detector import VADCommand, VADDetector

MODEL_PATH = os.path.join(SERVICE_DIR, "models", "silero_vad.onnx")
SOURCE_WAV = os.path.join(REPO_ROOT, "test_data", "input_1.wav")

TARGET_SR = 16000
FRAME_SIZE = 512
PAUSE_MS = 400

def load_and_resample(path: str) -> np.ndarray:
    with wave.open(path, "rb") as wf:
        original_sr = wf.getframerate()
        raw_bytes = wf.readframes(wf.getnframes())

    audio_float = np.frombuffer(raw_bytes, dtype=np.int16).astype(np.float32) / 32768.0

    if original_sr == TARGET_SR:
        return audio_float

    gcd = np.gcd(TARGET_SR, original_sr)
    return scipy.signal.resample_poly(
        audio_float, TARGET_SR // gcd, original_sr // gcd
    ).astype(np.float32)

def main():
    if not os.path.exists(MODEL_PATH):
        raise FileNotFoundError(f"Silero model file not found: {MODEL_PATH}")
    if not os.path.exists(SOURCE_WAV):
        raise FileNotFoundError(f"Source WAV not found: {SOURCE_WAV}")

    audio = load_and_resample(SOURCE_WAV)
    
    chunk_1 = audio[:int(1.5 * TARGET_SR)]
    chunk_2 = audio[int(2.0 * TARGET_SR) : int(3.5 * TARGET_SR)]
    
    silence = np.zeros(int((PAUSE_MS / 1000) * TARGET_SR), dtype=np.float32)
    
    trailing_silence = np.zeros(int(1.0 * TARGET_SR), dtype=np.float32)
    synthetic_audio = np.concatenate([chunk_1, silence, chunk_2, trailing_silence])
    num_frames = len(synthetic_audio) // FRAME_SIZE
    
    print(f"Synthesized audio: {len(synthetic_audio) / TARGET_SR:.2f} seconds")
    print(f"Injected digital silence: {PAUSE_MS}ms (VAD threshold is 500ms)\n")
    
    detector = VADDetector(MODEL_PATH)

    for i in range(num_frames):
        start = i * FRAME_SIZE
        end = start + FRAME_SIZE
        frame = synthetic_audio[start:end]
        
        command = detector.process_frames(frame)
        
        if command != VADCommand.NONE:
            timestamp_ms = i * detector.FRAME_DURATION_MS
            print(f"frame {i:3d} ({timestamp_ms:6.0f}ms): {command.name}")

if __name__ == "__main__":
    main()