# Voice Presets

This directory contains pre-built voice reference clips for LuxTTS.

## Shipped Presets

Voice presets should be .wav files (5-15 seconds of clean speech).
They are loaded on startup and available via the `voice` parameter in the TTS API.

### Adding Presets

1. Source high-quality voice clips from LibriTTS-R (CC BY 4.0):
   https://huggingface.co/datasets/blaze999/libritts_r

2. Trim to 10-15 seconds of clear, single-speaker speech.

3. Save as WAV (any sample rate — will be resampled internally).

4. Name the file descriptively: `professional.wav`, `narrator.wav`, etc.

### Expected Files

```
voices/
├── default.wav        # Neutral, clear American English
├── professional.wav   # Formal, confident tone
├── friendly.wav       # Warm, conversational
├── narrator.wav       # Deep, documentary style
├── bright.wav         # Energetic, upbeat
└── custom/            # User-uploaded reference clips (runtime)
```
