#!/bin/bash
# Container entrypoint (Vast runtype=args): own sshd with the app's key (PUBKEY_B64), then model + TabbyAPI.
env | grep -E '^(CONTAINER_|VAST_|WATCHDOG_)' > /opt/llm/vast.env 2>/dev/null
mkdir -p /root/.ssh /run/sshd && chmod 700 /root/.ssh
echo "${PUBKEY_B64:-}" | base64 -d > /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys
ssh-keygen -A >/dev/null 2>&1
/usr/sbin/sshd -o PermitRootLogin=prohibit-password -o PasswordAuthentication=no -o ClientAliveInterval=30
bash /opt/llm/start.sh &
exec sleep infinity
