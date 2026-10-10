# syntax=docker/dockerfile:1
# Strata engine (pinned commit) + llmd + monitoring stack; QuickPod runs it from a template (launch mode docker, CMD llmd).
# Build context is the repository root: docker build -f image/Dockerfile .
# Model weights are NOT in the image: llmd downloads the pinned revision in model.json with SHA256 checks.
FROM golang:1.27 AS llmd
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/llmd cmd/llmd
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /llmd ./cmd/llmd

FROM nvidia/cuda:13.0.0-devel-ubuntu24.04
ENV DEBIAN_FRONTEND=noninteractive PYTHONUNBUFFERED=1 LANG=C.UTF-8
RUN apt-get update && apt-get install -y --no-install-recommends build-essential ca-certificates curl git libatomic1 libgomp1 \
        python3 python3-pip python3-venv unzip openssh-server procps \
    && rm -rf /var/lib/apt/lists/*

# The engine is compiled here the way Strata's own Dockerfile does it: docker build has no GPU, so the CUDA
# architectures are fixed (a fat binary with a cubin per arch: RTX 30 = 86, RTX 40 = 89, RTX 50 = 120).
# 61b3fb5 is engine 0.1.42 (v0.1.42): on RTX 3090 + Ryzen 9 5900X it answered 118 tok/s in Russian, 105 in English and
# 103 after a 26K-token prompt, against 110 / 100 / 89 of 0.1.41 (QuickPod, 2026-10-11).
ARG STRATA_COMMIT=61b3fb5dd3f1e8ec09cf7e4e05208bc6d3c46406
ARG CUDA_ARCHITECTURES=86;89;120
WORKDIR /opt/strata
RUN git init -q . && git fetch -q --depth 1 https://github.com/Niko1221/Strata "$STRATA_COMMIT" && git checkout -q FETCH_HEAD \
    && python3 -m venv .venv && .venv/bin/pip install --no-cache-dir -r requirements.txt
RUN CUDA_ARCHITECTURES="$CUDA_ARCHITECTURES" .venv/bin/python - <<'PYEOF'
import os, shutil
import setup

llama = setup.get_llama_cpp()  # llama.cpp at Strata's pinned commit; its gguf-py is what tools/iq_pack.py reads GGUF with
nvcc, _ = setup.find_nvcc()
setup.cmake_build(setup.ROOT, setup.ROOT / "build", "strata",
    # STRATA_PORTABLE: without it ggml is compiled for the CPU of the build host (GGML_NATIVE), and an engine built on
    # a runner with AVX-512 dies with "Illegal instruction" on a rented machine without it. Portable is the AVX2 set.
    ["-DSTRATA_ENABLE_CUDA=ON", "-DSTRATA_BUILD_TESTS=OFF", "-DSTRATA_PORTABLE=ON", f"-DCMAKE_CUDA_ARCHITECTURES={os.environ['CUDA_ARCHITECTURES']}",
     f"-DCMAKE_CUDA_COMPILER={nvcc}", f"-DSTRATA_GGML_DIR={llama}"], None, "build-strata.bat")
(setup.ROOT / "engine").mkdir(exist_ok=True)
shutil.copy2(setup.ROOT / "build" / setup.EXE, setup.ROOT / "engine" / setup.EXE)
shutil.rmtree(setup.ROOT / "build")
PYEOF

# Monitoring stack, all on 127.0.0.1; Grafana is reached through the app's SSH tunnel
ARG PROM=3.15.0
ARG NODE=1.12.1
ARG LOKI=3.7.8
ARG ALLOY=1.20.1
ARG GRAF=13.2.3
RUN curl -fsSL "https://github.com/prometheus/prometheus/releases/download/v${PROM}/prometheus-${PROM}.linux-amd64.tar.gz" | tar -xz -C /opt \
    && mv /opt/prometheus-${PROM}.linux-amd64 /opt/prometheus \
    && curl -fsSL "https://github.com/prometheus/node_exporter/releases/download/v${NODE}/node_exporter-${NODE}.linux-amd64.tar.gz" | tar -xz -C /opt \
    && mv /opt/node_exporter-${NODE}.linux-amd64 /opt/node_exporter \
    && mkdir -p /opt/loki /opt/alloy \
    && curl -fsSL -o /tmp/loki.zip "https://github.com/grafana/loki/releases/download/v${LOKI}/loki-linux-amd64.zip" \
    && unzip -q /tmp/loki.zip -d /opt/loki && mv /opt/loki/loki-linux-amd64 /opt/loki/loki \
    && curl -fsSL -o /tmp/alloy.zip "https://github.com/grafana/alloy/releases/download/v${ALLOY}/alloy-linux-amd64.zip" \
    && unzip -q /tmp/alloy.zip -d /opt/alloy && mv /opt/alloy/alloy-linux-amd64 /opt/alloy/alloy \
    && chmod +x /opt/loki/loki /opt/alloy/alloy && rm /tmp/*.zip \
    && curl -fsSL "https://dl.grafana.com/oss/release/grafana-${GRAF}.linux-amd64.tar.gz" | tar -xz -C /opt \
    && mv /opt/grafana-* /opt/grafana

COPY image/monitoring/ /opt/monitoring/
COPY image/model.json /opt/llm/model.json
COPY --from=llmd /llmd /usr/local/bin/llmd
WORKDIR /root
ENTRYPOINT []
CMD ["/usr/local/bin/llmd"]
