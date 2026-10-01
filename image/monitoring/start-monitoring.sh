#!/bin/bash
# Monitoring stack on the GPU machine. Everything listens on 127.0.0.1; Grafana is reached through the app's SSH tunnel.
# Each service is restarted if it exits. Data is kept at most 3 days (and disappears with the machine on Down).
L=/var/log/llm; mkdir -p $L/dialogs /var/lib/prometheus /var/lib/loki /var/lib/alloy /var/lib/grafana
# Grafana logs an error at startup for every provisioning directory that is missing
mkdir -p /opt/monitoring/provisioning/alerting /opt/monitoring/provisioning/plugins
forever() { local name=$1; shift; ( while true; do "$@" >> $L/$name.log 2>&1; echo "$(date) $name exited, restart in 5s" >> $L/$name.log; sleep 5; done ) & }
forever gpu_exporter python3 /opt/monitoring/gpu_exporter.py
# overlay is NOT excluded (default excludes it), so the container's root disk is visible
forever node_exporter /opt/node_exporter/node_exporter --web.listen-address=127.0.0.1:9100 \
  "--collector.filesystem.fs-types-exclude=^(autofs|binfmt_misc|bpf|cgroup2?|configfs|debugfs|devpts|devtmpfs|fusectl|hugetlbfs|iso9660|mqueue|nsfs|proc|procfs|pstore|rpc_pipefs|securityfs|selinuxfs|squashfs|sysfs|tracefs)$"
forever prometheus /opt/prometheus/prometheus --config.file=/opt/monitoring/prometheus.yml --storage.tsdb.path=/var/lib/prometheus \
  --storage.tsdb.retention.time=3d --web.listen-address=127.0.0.1:9090
forever loki /opt/loki/loki --config.file=/opt/monitoring/loki.yml
forever alloy /opt/alloy/alloy run --server.http.listen-addr=127.0.0.1:12345 --storage.path=/var/lib/alloy /opt/monitoring/config.alloy
export GF_SERVER_HTTP_ADDR=127.0.0.1 GF_SERVER_HTTP_PORT=3000 GF_PATHS_PROVISIONING=/opt/monitoring/provisioning GF_PATHS_DATA=/var/lib/grafana \
  GF_AUTH_ANONYMOUS_ENABLED=true GF_AUTH_ANONYMOUS_ORG_ROLE=Admin GF_AUTH_DISABLE_LOGIN_FORM=true GF_ANALYTICS_REPORTING_ENABLED=false \
  GF_ANALYTICS_CHECK_FOR_UPDATES=false GF_NEWS_NEWS_FEED_ENABLED=false GF_USERS_DEFAULT_LANGUAGE=ru-RU GF_LOG_LEVEL=warn \
  GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/opt/monitoring/dashboards/llm-metrics.json
forever grafana /opt/grafana/bin/grafana server --homepath /opt/grafana
