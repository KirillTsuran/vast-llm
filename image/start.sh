#!/bin/bash
# Vast onstart: download pinned weights, start TabbyAPI on 127.0.0.1:8080 (reachable only via the SSH tunnel),
# and the heartbeat watchdog (the app passes WATCHDOG_MIN=1 year: machines are removed only by the Down button).
L=/var/log/llm; S=/opt/llm/state; mkdir -p $L; set -a; . /opt/llm/vast.env 2>/dev/null; set +a
pgrep -f "python serve.py" >/dev/null && exit 0
# monitoring first, so GPU panels work during the model download
python3 /opt/monitoring/gpu_exporter.py > $L/gpu_exporter.log 2>&1 &
/opt/prometheus/prometheus --config.file=/opt/monitoring/prometheus.yml --storage.tsdb.path=/var/lib/prometheus   --storage.tsdb.retention.time=2d --web.listen-address=127.0.0.1:9090 > $L/prometheus.log 2>&1 &
GF_SERVER_HTTP_ADDR=127.0.0.1 GF_SERVER_HTTP_PORT=3000 GF_PATHS_PROVISIONING=/opt/monitoring/provisioning GF_PATHS_DATA=/var/lib/grafana GF_AUTH_ANONYMOUS_ENABLED=true GF_AUTH_ANONYMOUS_ORG_ROLE=Admin GF_AUTH_DISABLE_LOGIN_FORM=true GF_ANALYTICS_REPORTING_ENABLED=false GF_ANALYTICS_CHECK_FOR_UPDATES=false GF_NEWS_NEWS_FEED_ENABLED=false GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/opt/monitoring/dashboards/llm.json   /opt/grafana/bin/grafana server --homepath /opt/grafana > $L/grafana.log 2>&1 &
echo downloading > $S
if ! python3 /opt/llm/download.py qwen3.8-27b-uncensored dflash > $L/download.log 2>&1; then echo download-failed > $S; exit 1; fi
python3 - <<'PY'
import json; p='/opt/llm/models/qwen3.8-27b-uncensored/tokenizer_config.json'
d=json.load(open(p)); d['add_bos_token']=False; json.dump(d, open(p,'w'), indent=2)   # production BOS fix
PY
echo loading > $S
cd /app && nohup env LLM_LOG_DIR=$L/dialogs python serve.py > $L/tabby.log 2>&1 &
( for i in $(seq 240); do curl -sf http://127.0.0.1:8080/v1/model >/dev/null && { echo ready > $S; break; }; sleep 5; done ) &
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
