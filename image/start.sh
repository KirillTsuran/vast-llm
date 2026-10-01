#!/bin/bash
# Vast onstart: download pinned weights, start TabbyAPI on 127.0.0.1:8080 (reachable only via the SSH tunnel),
# and the heartbeat watchdog (the app passes WATCHDOG_MIN=1 year: machines are removed only by the Down button).
L=/var/log/llm; S=/opt/llm/state; mkdir -p $L; set -a; . /opt/llm/vast.env 2>/dev/null; set +a
pgrep -f "python serve.py" >/dev/null && exit 0
# monitoring first, so its panels work during the model download
bash /opt/monitoring/start-monitoring.sh
echo downloading > $S
if ! python3 /opt/llm/download.py qwen3.8-27b-uncensored dflash > $L/download.log 2>&1; then echo download-failed > $S; exit 1; fi
python3 - <<'PY'
import json; p='/opt/llm/models/qwen3.8-27b-uncensored/tokenizer_config.json'
d=json.load(open(p)); d['add_bos_token']=False; json.dump(d, open(p,'w'), indent=2)   # production BOS fix
PY
echo loading > $S
# TabbyAPI is restarted if it ever exits (CUDA error, out of memory, crash); the reason stays in tabby.log
( cd /app; while true; do
    LLM_LOG_DIR=$L/dialogs python serve.py >> $L/tabby.log 2>&1; rc=$?
    echo "$(date '+%F %T') TabbyAPI exited with code $rc, restart in 10s" >> $L/tabby.log; echo restarting > $S; sleep 10
  done ) > /dev/null 2>&1 &
# state follows the API: ready whenever the model answers
( while sleep 5; do
    if curl -sf -m 4 http://127.0.0.1:8080/v1/model > /dev/null; then [ "$(cat $S 2>/dev/null)" = ready ] || echo ready > $S; fi
  done ) > /dev/null 2>&1 &
# watchdog: the app touches /opt/llm/heartbeat every minute; after WATCHDOG_MIN minutes without it, destroy self.
touch /opt/llm/heartbeat
( while sleep 60; do
    age=$(( $(date +%s) - $(stat -c %Y /opt/llm/heartbeat) ))
    if [ "$age" -gt $(( ${WATCHDOG_MIN:-60} * 60 )) ]; then
      echo "$(date) no heartbeat ${age}s, self-destroy" >> $L/watchdog.log
      curl -s -X DELETE -H "Authorization: Bearer ${CONTAINER_API_KEY:-}" "https://console.vast.ai/api/v0/instances/${CONTAINER_ID:-0}/" >> $L/watchdog.log 2>&1
      sleep 600
    fi
  done ) > /dev/null 2>&1 &
