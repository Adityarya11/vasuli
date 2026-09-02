"""Confirms max_utterance_sec forces an END_OF_UTTERANCE even under
continuous speech with no silence gap, i.e. the ramble ceiling fires
independent of the normal silence-based debounce path.
"""

import os
import sys

import numpy as np

TEST_DIR = os.path.dirname(os.path.abspath(__file__))
SERVICE_DIR = os.path.abspath(os.path.join(TEST_DIR, ".."))

if SERVICE_DIR not in sys.path:
    sys.path.insert(0, SERVICE_DIR)

from vad import VADCommand, VADDetector

VAD_MODEL_PATH = os.path.join(SERVICE_DIR, "models", "silero_vad.onnx")


def test_ramble_max_ceiling():
    if not os.path.exists(VAD_MODEL_PATH):
        raise FileNotFoundError(f"Silero model file not found: {VAD_MODEL_PATH}")

    # Configure with a tight max_utterance_sec of 5 seconds to speed up testing
    # 5 seconds @ 16kHz with 512 frame sizes = exactly 156.25 -> 156 frames max ceiling
    max_sec = 5.0
    detector = VADDetector(VAD_MODEL_PATH, max_utterance_sec=max_sec)
    expected_max_frames = detector._max_utterance_frames

    # Mock infer to permanently simulate active human speech (score = 0.95)
    original_infer = detector._infer
    detector._infer = lambda frame: (0.95, detector._state)

    start_speech_idx = -1
    end_utterance_idx = -1
    start_speech_count = 0
    end_utterance_count = 0

    # Run for 200 continuous speech frames to comfortably overshoot the 156 ceiling
    for idx in range(200):
        dummy_frame = np.zeros(512, dtype=np.float32)
        command = detector.process_frames(dummy_frame)
        
        if command == VADCommand.START_SPEECH:
            start_speech_count += 1
            start_speech_idx = idx
        elif command == VADCommand.END_OF_UTTERANCE:
            end_utterance_count += 1
            end_utterance_idx = idx
            break  # Stop streaming once cutoff triggers

    print(f"[TEST RAMBLE] Expected ceiling frame index: {expected_max_frames}")
    print(f"[TEST RAMBLE] START_SPEECH fired at frame index: {start_speech_idx}")
    print(f"[TEST RAMBLE] END_OF_UTTERANCE forced at frame index: {end_utterance_idx}")

    # Assertions
    assert start_speech_count == 1, "START_SPEECH must fire exactly once upon crossing the entry threshold."
    assert end_utterance_count == 1, "Ceiling failed to trigger an END_OF_UTTERANCE command."
    
    # The absolute frame index where END_OF_UTTERANCE triggers must align with expected ceiling
    # because _utterance_frames appends frames every loop after entry validation.
    assert end_utterance_idx == expected_max_frames - 1, f"Ceiling mismatch! Cutoff fired at loop index {end_utterance_idx}, expected {expected_max_frames - 1}"
        
    print("PASS: test_vad_ramble")

if __name__ == "__main__":
    test_ramble_max_ceiling()