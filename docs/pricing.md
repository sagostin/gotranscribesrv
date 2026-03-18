# Pricing & Cost Analysis

> All prices in Canadian dollars (CAD).

## Infrastructure Cost Model

GoTranscribeSrv runs entirely on-premise on Mac Mini hardware. There are no per-request API fees, no cloud compute costs, and no data leaving your network.

---

## Hardware Configurations

### Per-Node Options

| Config | Chip | RAM | Storage | Est. Price (CAD) | Max Concurrent Streams | Best For |
|--------|------|-----|---------|------------|----------------------|----------|
| **Starter** | M4 | 16 GB | 256 GB | ~$700 | 3–5 | Dev/staging, 0.6B model |
| **Standard** | M4 | 24 GB | 256 GB | ~$950 | 5–8 | ASR + TTS + diarization |
| **Recommended** | M4 | 32 GB | 256 GB | ~$1,150 | 5–8 | Full stack incl. LLM processing |
| **Pro** | M4 Pro | 48 GB | 512 GB | ~$1,900 | 8–12 | Heavy concurrent LLM + ASR |

### Cluster Configurations

| Deployment | Nodes | Hardware Cost (CAD) | Monthly Power | Concurrent Streams | Throughput (file) |
|-----------|-------|-------------|---------------|-------------------|-------------------|
| **Solo Dev** | 1× Standard | $950 | ~$7 | 5–8 | ~5–6 req/sec |
| **Small Team** | 2× Recommended | $2,300 | ~$14 | 10–16 | ~10–12 req/sec |
| **Production** | 3× Recommended + LB | $3,700 | ~$20 | 15–24 | ~15–18 req/sec |
| **Growth** | 5× Recommended + LB + PG host | $6,700 | ~$35 | 25–40 | ~25–30 req/sec |
| **Scale** | 10× Recommended + LB + PG host | $13,000 | ~$70 | 50–80 | ~50–60 req/sec |

> **Power costs** based on Mac Mini M4 ~20W average under ML load × 24/7 × $0.13/kWh (Canadian avg).

---

## Breakeven vs Cloud ASR

### Cloud Pricing (per audio hour in CAD, as of 2026)

| Provider | Standard ASR | With Diarization | With Timestamps |
|----------|-------------|-------------------|-----------------|
| Google Speech-to-Text v2 | $2.00 | $2.30 | included |
| AWS Transcribe | $2.00 | included | included |
| Azure Speech | $1.40 | +$0.55 | included |
| Deepgram (Nova-2) | $1.05 | +$0.35 | included |
| AssemblyAI | $0.90 | included | included |
| OpenAI Whisper API | $0.50 | not available | not available |

### Breakeven Analysis

| Monthly Volume | Cheapest Cloud (OpenAI) | Mid-Tier (Deepgram) | Premium (Google) | GoTranscribeSrv (3-node) |
|---------------|------------------------|--------------------|--------------------|--------------------------| 
| 100 hrs | $50 | $105 | $200 | $3,100 one-time + $20/mo |
| 500 hrs | $250 | $525 | $1,000 | same |
| 1,000 hrs | $500 | $1,050 | $2,000 | same |
| 5,000 hrs | $2,500 | $5,250 | $10,000 | same |
| 10,000 hrs | $5,000 | $10,500 | $20,000 | same |

**Breakeven points (3-node Standard cluster @ $3,100 CAD):**

| vs Provider | Monthly Volume for 6-Month Payback | Monthly Volume for 12-Month Payback |
|------------|-----------------------------------|-------------------------------------|
| Google ($2.00/hr) | ~260 hrs/mo | ~130 hrs/mo |
| Deepgram ($1.05/hr) | ~500 hrs/mo | ~250 hrs/mo |
| OpenAI ($0.50/hr) | ~1,050 hrs/mo | ~525 hrs/mo |

> A 3-node cluster can process **~43,200 hours of audio per month** continuously (24/7, ~70x real-time, 3 nodes). The hardware pays for itself rapidly at virtually any meaningful volume.

---

## Total Cost of Ownership (Year 1)

### Scenario: 1,000 audio hours/month

| Item | GoTranscribeSrv | Google Cloud | Deepgram |
|------|----------------|-------------|----------|
| Hardware (one-time) | $3,100 | $0 | $0 |
| Monthly compute | $20 (power) | $2,000 | $1,050 |
| Annual compute | $240 | $24,000 | $12,600 |
| **Year 1 total** | **$3,340** | **$24,000** | **$12,600** |
| **Year 2 total** | **$3,580** | **$48,000** | **$25,200** |

### Scenario: 10,000 audio hours/month

| Item | GoTranscribeSrv (10-node) | Google Cloud | Deepgram |
|------|--------------------------|-------------|----------|
| Hardware (one-time) | $11,000 | $0 | $0 |
| Annual compute | $840 (power) | $240,000 | $126,000 |
| **Year 1 total** | **$11,840** | **$240,000** | **$126,000** |

---

## What You Get vs Cloud

| Capability | GoTranscribeSrv | Cloud ASR |
|-----------|----------------|-----------|
| Per-request cost | $0 | $0.50–2.00/hr |
| Data privacy | 100% on-premise | Data sent to cloud |
| Latency | <1ms to inference | 50–200ms network |
| Diarization | Included | +$0.35–0.55/hr |
| Timestamps | Included | Usually included |
| Streaming ASR | Included | Often extra |
| Custom model fine-tuning | Full control | Limited |
| Internet dependency | None | Required |
| Scaling | Add hardware | Auto (but costs scale) |

---

## Capacity Planning Calculator

```
Concurrent streams needed:     ___
÷ 6 (streams per Standard node): ___  → number of nodes needed

Audio hours per month:          ___
÷ 4,320 (hrs/node/month at 70x RT): ___  → nodes for throughput

Take the higher number. Add 1 for headroom.

Hardware cost = nodes × $1,150 CAD (Recommended w/ LLM)
Monthly power = nodes × $7 CAD
```

### Example

> **Need:** 20 concurrent streams, ~8,000 hrs/month
>
> Concurrent: 20 ÷ 6 = 4 nodes
> Throughput: 8,000 ÷ 4,320 = 2 nodes
> → **4 nodes + 1 headroom = 5× Recommended ($5,750 CAD)**
> Monthly power: ~$35
