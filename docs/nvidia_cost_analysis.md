# Mac Mini vs NVIDIA GPU: Cost Analysis

> All prices in Canadian dollars (CAD). Prices as of March 2026.

## Executive Summary

The Mac Mini approach offers **dramatically lower total cost** for GoTranscribeSrv's current workloads. An NVIDIA GPU path delivers significantly higher raw throughput per node but comes with **10–20× higher hardware cost**, **25–50× higher power consumption**, complex infrastructure requirements, and partial software stack incompatibility (Parakeet MLX won't run on CUDA — the NeMo/PyTorch CUDA path must be used instead).

> [!IMPORTANT]
> The NVIDIA path would require migrating from **parakeet-mlx** to **NeMo's native CUDA inference** (or TensorRT). This is a non-trivial engineering effort but unlocks massive batch throughput.

---

## GPU Options Compared

### Hardware Specs

| Spec | Mac Mini M4 (24GB/512GB) | Mac Mini M4 Pro (48GB) | RTX 5080 | RTX 4090 | RTX 5090 |
|------|-------------------|----------------------|----------|----------|----------|
| **Price (CAD)** | $1,399 | ~$1,900 | ~$1,800 (GPU only) | ~$2,700 (GPU only) | ~$3,600 (GPU only) |
| **VRAM / Memory** | 24 GB unified | 48 GB unified | 16 GB GDDR7 | 24 GB GDDR6X | 32 GB GDDR7 |
| **AI TOPS (INT8)** | ~38 | ~67 | ~1,801 | ~1,321 | ~3,352 |
| **TDP** | ~20W (whole system) | ~35W (whole system) | 250W (GPU only) | 450W (GPU only) | 575W (GPU only) |
| **Form Factor** | Plug-and-play mini | Plug-and-play mini | Needs full PC build | Needs full PC build | Needs full PC build |

### ASR Performance (Parakeet TDT 1.1B)

| Platform | Framework | Speed vs Real-Time | 1 hr audio processed in | Batch Support |
|----------|-----------|-------------------|------------------------|---------------|
| Mac Mini M4 (MLX) | parakeet-mlx | ~40–60× | ~60–90 sec | Single stream |
| Mac Mini M4 Pro (CoreML) | parakeet-coreml | ~60–110× | ~33–60 sec | Single stream |
| RTX 4090 (CUDA) | NeMo + PyTorch | ~150–300× | ~12–24 sec | ✅ Batched (up to 128) |
| RTX 5090 (CUDA) | NeMo + PyTorch | ~200–450× | ~8–18 sec | ✅ Batched (up to 128) |
| RTX 5090 (TensorRT) | NeMo + TRT | ~400–800× | ~4.5–9 sec | ✅ Batched (up to 128) |

> **Key insight:** NVIDIA GPUs can batch multiple audio files simultaneously, which is where the throughput advantage becomes massive. A single RTX 5090 with TensorRT could match or exceed **an entire 10-node Mac Mini cluster** for file-based ASR throughput.

---

## Full System Build Costs (NVIDIA)

An NVIDIA GPU can't run on its own — it needs a full host machine. Here are realistic build costs:

### Single-GPU Workstation

| Component | RTX 4090 Build | RTX 5080 Build | RTX 5090 Build |
|-----------|---------------|----------------|----------------|
| GPU | $2,700 | $1,800 | $3,600 |
| CPU (Ryzen 9 / i9) | $600 | $600 | $600 |
| Motherboard | $400 | $400 | $400 |
| RAM (64 GB DDR5) | $250 | $250 | $250 |
| NVMe SSD (1 TB) | $120 | $120 | $120 |
| PSU (1000W 80+ Gold) | $200 | $180 | $250 |
| Case + Cooling | $200 | $200 | $250 |
| **Total** | **~$4,470** | **~$3,550** | **~$5,470** |

### Multi-GPU Server (2× GPU)

| Component | 2× RTX 4090 | 2× RTX 5090 |
|-----------|-------------|-------------|
| GPUs | $5,400 | $7,200 |
| CPU (Threadripper) | $1,400 | $1,400 |
| Workstation Motherboard | $800 | $800 |
| RAM (128 GB DDR5 ECC) | $500 | $500 |
| NVMe SSD (2 TB) | $200 | $200 |
| PSU (1600W 80+ Plat.) | $400 | $450 |
| Rack Case + Cooling | $400 | $500 |
| **Total** | **~$9,100** | **~$11,050** |

> [!WARNING]
> Quad-GPU builds (4× RTX 5090) require dual PSU setups, 30A/240V circuits, and specialized cooling. Total cost: **$25,000–$30,000+** CAD. This enters data center territory.

---

## Power Consumption & Monthly Costs

| Setup | Avg. Power Draw | Monthly Power Cost* | Annual Power |
|-------|----------------|--------------------:|-------------:|
| 1× Mac Mini M4 (24GB/512GB) | ~20W | **$1.90** | $23 |
| 3× Mac Mini M4 (24GB/512GB) | ~60W | **$5.70** | $68 |
| 10× Mac Mini M4 (24GB/512GB) | ~200W | **$19** | $228 |
| 1× RTX 4090 system | ~550W | **$52** | $624 |
| 1× RTX 5090 system | ~700W | **$66** | $792 |
| 2× RTX 4090 system | ~1,100W | **$104** | $1,248 |
| 2× RTX 5090 system | ~1,400W | **$133** | $1,596 |

*Based on 24/7 operation at $0.13/kWh Canadian average.

---

## Cost Comparison at Scale

### Scenario A: Small Deployment (5–8 concurrent streams)

| | Mac Mini (1× M4 24GB/512GB) | NVIDIA (1× RTX 5080) | NVIDIA (1× RTX 4090) |
|-|----------------------|---------------------|---------------------|
| Hardware | $1,399 | $3,550 | $4,470 |
| Year 1 Power | $23 | $570 | $624 |
| **Year 1 Total** | **$1,422** | **$4,120** | **$5,094** |
| Year 2 cumulative | $1,445 | $4,690 | $5,718 |
| Throughput (file ASR) | ~5–6 req/s | ~15–20 req/s | ~25–35 req/s |

### Scenario B: Production (15–24 concurrent streams)

| | Mac Mini (3× M4 24GB/512GB) | NVIDIA (1× RTX 5090) | NVIDIA (2× RTX 4090) |
|-|----------------------|---------------------|---------------------|
| Hardware | $4,197 | $5,470 | $9,100 |
| Year 1 Power | $68 | $792 | $1,248 |
| **Year 1 Total** | **$4,265** | **$6,262** | **$10,348** |
| Year 2 cumulative | $4,333 | $7,054 | $11,596 |
| Throughput (file ASR) | ~15–18 req/s | ~40–60 req/s | ~50–70 req/s |
| Max audio hrs/month (24/7) | ~43,200 | ~86,000+ | ~100,000+ |

### Scenario C: High Scale (50+ concurrent streams)

| | Mac Mini (10× M4 24GB/512GB) | NVIDIA (2× RTX 5090) | NVIDIA (4× RTX 4090) |
|-|------------------------|---------------------|---------------------|
| Hardware | $13,990 | $11,050 | ~$18,500 |
| Year 1 Power | $228 | $1,596 | $2,496 |
| **Year 1 Total** | **$14,218** | **$12,646** | **~$20,996** |
| Year 2 cumulative | $14,446 | $14,242 | ~$23,492 |
| Throughput (file ASR) | ~50–60 req/s | ~80–120 req/s | ~100–140 req/s |

---

## Additional Costs & Considerations

### Things You'd Need for the NVIDIA Path

| Item | Cost | Notes |
|------|------|-------|
| **Software Migration** | 40–80 hrs eng time | Migrate from parakeet-mlx to NeMo CUDA. Diarization and TTS already use PyTorch, so those are easier. |
| **TensorRT Optimization** | 20–40 hrs eng time | Optional but recommended for max throughput. Compile Parakeet TDT to TRT engine. |
| **CUDA/cuDNN Licensing** | $0 | Free for inference |
| **Ubuntu Server** | $0 | Free OS |
| **Rack/Colocation** | $100–500/mo | If not running in-office. Noise and heat are significant. |
| **UPS / Power Infrastructure** | $300–1,500 | High-wattage UPS for 700W–1400W+ draw |
| **Networking** | ~$0–200 | Same as Mac Mini cluster |
| **Noise** | ⚠️ Significant | GPU servers are **loud** (50–60 dB). Mac Minis are silent. Office-unfriendly. |
| **Physical Space** | Full tower/rack | vs. Mac Mini's 5" × 5" footprint |

### Feature Compatibility Matrix

| Feature | Mac Mini (MLX) | NVIDIA (CUDA) |
|---------|---------------|---------------|
| Parakeet TDT ASR | ✅ via parakeet-mlx | ✅ via NeMo (requires migration) |
| Streaming ASR | ✅ | ✅ (would need re-integration) |
| Speaker Diarization (Sortformer) | ✅ PyTorch/MPS | ✅ PyTorch/CUDA (native) |
| TTS (LuxTTS) | ✅ PyTorch/MPS | ✅ PyTorch/CUDA (native) |
| VAD (Silero) | ✅ ONNX/CoreML | ✅ ONNX/CUDA |
| LLM Processing (Llama 8B) | ✅ via mlx-lm | ✅ via vLLM/TRT-LLM (faster) |
| Batch inference | ❌ Single stream | ✅ Up to 128 concurrent |
| Zero config / plug and play | ✅ | ❌ Requires Linux admin |

---

## When Does NVIDIA Make Sense?

### ✅ Choose NVIDIA GPU if:
- **Throughput is paramount** — You need 100,000+ audio hours/month from minimal hardware
- **Batch processing** is the dominant workload (not real-time streaming)
- **You already have** GPU server infrastructure, Linux ops expertise, and rack space
- **Future model scaling** — You want to run larger ASR models (2B+) or do model fine-tuning
- **LLM processing** is a major feature — CUDA is significantly faster for LLM inference

### ✅ Choose Mac Mini if:
- **Cost efficiency** is the priority — 3–5× cheaper Year 1, 5–10× cheaper over 3 years
- **Simplicity** matters — Zero Linux admin, no driver management, no cooling headaches
- **Office deployment** — Silent, tiny, low power, no special circuits needed
- **Real-time streaming** is the main use case (single-stream latency is comparable)
- **Data privacy** is paramount — Each node is a self-contained appliance
- **Team is small** — No dedicated infra/ops engineer needed

---

## Bottom Line

| Metric | Mac Mini (10-node) | NVIDIA (2× RTX 5090) |
|--------|-------------------|---------------------|
| Hardware Cost | $13,990 | $11,050 |
| Year 1 Total | **$14,218** | **$12,646** |
| Year 3 Total | **$14,674** | **$15,838** |
| Peak Throughput | ~50–60 req/s | ~80–120 req/s |
| Power (monthly) | $19 | $133 |
| Engineering Migration | $0 | 60–120 hrs |
| Noise | Silent | Loud |
| Physical Footprint | 10 × (5" cubes) | Full tower/rack |
| Maintenance | Almost none | GPU drivers, cooling, Linux admin |

**For GoTranscribeSrv's current architecture and scale, Mac Mini remains the better choice by a wide margin.** The NVIDIA path would only make sense if you need to process enormous batch volumes (100K+ hrs/month) from minimal physical hardware, or if you're planning to shift toward fine-tuning and training workloads. The engineering effort to migrate from MLX to CUDA is non-trivial and eliminates the current "plug and play" advantage.
