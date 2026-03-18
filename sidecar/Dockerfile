# ============================================================
# Python Inference Sidecar
# ============================================================
FROM python:3.11-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    libsndfile1 \
    ffmpeg \
    git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Install Python dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Clone LuxTTS and install (zipvoice.luxvoice)
# linacodec and piper-phonemize need special sources that pip can't resolve from PyPI
RUN git clone --depth 1 https://github.com/ysharma3501/LuxTTS.git /app/LuxTTS \
    && pip install --no-cache-dir "linacodec @ git+https://github.com/ysharma3501/LinaCodec.git" \
    && pip install --no-cache-dir piper-phonemize --find-links https://k2-fsa.github.io/icefall/piper_phonemize.html \
    && pip install --no-cache-dir /app/LuxTTS

# Add LuxTTS to Python path so 'from zipvoice.luxvoice import LuxTTS' works
ENV PYTHONPATH="/app/LuxTTS:${PYTHONPATH}"

# Copy application code
COPY . .

EXPOSE 8100

# Healthcheck is managed by docker-compose.yml with generous timeouts
# suitable for long-running ML inference (30s timeout, 120s start_period).
# Do NOT add a Dockerfile-level HEALTHCHECK — it overrides compose settings
# and will kill the container during long transcription requests.

CMD ["python3", "main.py"]
