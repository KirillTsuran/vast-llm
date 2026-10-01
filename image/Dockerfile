# Production TabbyAPI build (base 816c321 + production overlays) + SS_LoopBreak, for Vast.ai (runtype ssh, onstart=/opt/llm/start.sh).
# Model weights are NOT in the image: start.sh downloads pinned HF revisions with SHA256 checks.
FROM python:3.12-slim-bookworm
ENV PIP_NO_CACHE_DIR=1 PYTHONUNBUFFERED=1 HF_HUB_DISABLE_TELEMETRY=1
RUN apt-get update && apt-get install -y --no-install-recommends gcc g++ curl ca-certificates libgomp1 aria2 openssh-server procps \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
ADD tabby-source.tgz /app/
ARG EXL=exllamav3-1.5.3+cu128.torch2.11.0-cp312-cp312-linux_x86_64.whl
RUN pip install torch==2.11.0 --index-url https://download.pytorch.org/whl/cu128 \
    && curl -fL -o /tmp/$EXL "https://github.com/turboderp-org/exllamav3/releases/download/v1.5.3/exllamav3-1.5.3%2Bcu128.torch2.11.0-cp312-cp312-linux_x86_64.whl" \
    && echo "0cf0052ed8237218fc38d24e49a1cf9fffb3186851e07016de550fb696bc12e6  /tmp/$EXL" | sha256sum -c - \
    && pip install /tmp/$EXL transformers==5.17.0 . && rm /tmp/$EXL && pip freeze > /app/packages.lock
COPY overlay/ /tmp/overlay/
COPY loopbreak.py patch_tabby.py /tmp/lb/
RUN SP=$(python -c 'import exllamav3,os;print(os.path.dirname(exllamav3.__file__))') \
    && cp /tmp/overlay/job.py $SP/generator/job.py \
    && cp /tmp/overlay/model.py backends/exllamav3/model.py \
    && cp /tmp/overlay/chat_completion.py endpoints/OAI/utils/chat_completion.py \
    && cp /tmp/overlay/errors.py common/errors.py && cp /tmp/overlay/auth.py common/auth.py \
    && cp /tmp/overlay/observability.py /tmp/overlay/serve.py /app/ \
    && python /tmp/lb/patch_tabby.py /app && rm -rf /tmp/overlay /tmp/lb
COPY tabby-config.yml /app/config.yml
COPY start.sh entry.sh download.py /opt/llm/
COPY manifests/ /opt/llm/manifests/
RUN chmod +x /opt/llm/start.sh /opt/llm/entry.sh
