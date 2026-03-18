# Pricing & Cost Analysis

## Infrastructure Cost Model

GoTranscribeSrv runs entirely on-premise on Mac Mini hardware. There are no per-request API fees, no cloud compute costs, and no data leaving your network.

---

## Hardware Configurations

### Per-Node Options

| Config | Chip | RAM | Storage | Est. Price | Max Concurrent Streams | Best For |
|--------|------|-----|---------|------------|----------------------|----------|
| **Starter** | M4 | 16 GB | 256 GB | ~$500 | 3–5 | Dev/staging, 0.6B model |
| **Standard** | M4 | 24 GB | 512 GB | ~$700 | 5–8 | Production, 1.1B model |
| **Pro** | M4 Pro | 48 GB | 512 GB | ~$1,400 | 8–12 | High-throughput, all features |

### Cluster Configurations

| Deployment | Nodes | Hardware Cost | Monthly Power | Concurrent Streams | Throughput (file) |
|-----------|-------|-------------|---------------|-------------------|-------------------|
| **Solo Dev** | 1× Standard | $700 | ~$5 | 5–8 | ~5–6 req/sec |
| **Small Team** | 2× Standard | $1,400 | ~$10 | 10–16 | ~10–12 req/sec |
| **Production** | 3× Standard + LB | $2,300 | ~$15 | 15–24 | ~15–18 req/sec |
| **Growth** | 5× Standard + LB + PG host | $4,200 | ~$25 | 25–40 | ~25–30 req/sec |
| **Scale** | 10× Standard + LB + PG host | $8,000 | ~$50 | 50–80 | ~50–60 req/sec |

> **Power costs** based on Mac Mini M4 ~20W average under ML load × 24/7 × $0.12/kWh.

---

## Breakeven vs Cloud ASR

### Cloud Pricing (per audio hour, as of 2026)

| Provider | Standard ASR | With Diarization | With Timestamps |
|----------|-------------|-------------------|-----------------|
| Google Speech-to-Text v2 | $1.44 | $1.68 | included |
| AWS Transcribe | $1.44 | included | included |
| Azure Speech | $1.00 | +$0.40 | included |
| Deepgram (Nova-2) | $0.75 | +$0.24 | included |
| AssemblyAI | $0.65 | included | included |
| OpenAI Whisper API | $0.36 | not available | not available |

### Breakeven Analysis

| Monthly Volume | Cheapest Cloud (OpenAI) | Mid-Tier (Deepgram) | Premium (Google) | GoTranscribeSrv (3-node) |
|---------------|------------------------|--------------------|--------------------|--------------------------|
| 100 hrs | $36 | $75 | $144 | $2,300 one-time + $15/mo |
| 500 hrs | $180 | $375 | $720 | same |
| 1,000 hrs | $360 | $750 | $1,440 | same |
| 5,000 hrs | $1,800 | $3,750 | $7,200 | same |
| 10,000 hrs | $3,600 | $7,500 | $14,400 | same |

**Breakeven points (3-node Standard cluster @ $2,300):**

| vs Provider | Monthly Volume for 6-Month Payback | Monthly Volume for 12-Month Payback |
|------------|-----------------------------------|-------------------------------------|
| Google ($1.44/hr) | ~300 hrs/mo | ~150 hrs/mo |
| Deepgram ($0.75/hr) | ~580 hrs/mo | ~290 hrs/mo |
| OpenAI ($0.36/hr) | ~1,200 hrs/mo | ~600 hrs/mo |

> A 3-node cluster can process **~43,200 hours of audio per month** continuously (24/7, ~70x real-time, 3 nodes). The hardware pays for itself rapidly at virtually any meaningful volume.

---

## Total Cost of Ownership (Year 1)

### Scenario: 1,000 audio hours/month

| Item | GoTranscribeSrv | Google Cloud | Deepgram |
|------|----------------|-------------|----------|
| Hardware (one-time) | $2,300 | $0 | $0 |
| Monthly compute | $15 (power) | $1,440 | $750 |
| Annual compute | $180 | $17,280 | $9,000 |
| **Year 1 total** | **$2,480** | **$17,280** | **$9,000** |
| **Year 2 total** | **$2,660** | **$34,560** | **$18,000** |

### Scenario: 10,000 audio hours/month

| Item | GoTranscribeSrv (10-node) | Google Cloud | Deepgram |
|------|--------------------------|-------------|----------|
| Hardware (one-time) | $8,000 | $0 | $0 |
| Annual compute | $600 (power) | $172,800 | $90,000 |
| **Year 1 total** | **$8,600** | **$172,800** | **$90,000** |

---

## What You Get vs Cloud

| Capability | GoTranscribeSrv | Cloud ASR |
|-----------|----------------|-----------|
| Per-request cost | $0 | $0.36–1.44/hr |
| Data privacy | 100% on-premise | Data sent to cloud |
| Latency | <1ms to inference | 50–200ms network |
| Diarization | Included | +$0.24–0.40/hr |
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

Hardware cost = nodes × $700
Monthly power = nodes × $5
```

### Example

> **Need:** 20 concurrent streams, ~8,000 hrs/month
>
> Concurrent: 20 ÷ 6 = 4 nodes
> Throughput: 8,000 ÷ 4,320 = 2 nodes
> → **4 nodes + 1 headroom = 5× Standard ($3,500)**
> Monthly power: ~$25
