#!/bin/bash
# Start/restart bunkerd on Hetzner server

pkill -f "/usr/local/bin/bunkerd --config" 2>/dev/null || true
sleep 2
nohup /usr/local/bin/bunkerd --config /etc/bunkerd/config.yaml > /var/log/bunkerd.log 2>&1 < /dev/null &
sleep 2
if pgrep -f "/usr/local/bin/bunkerd --config" >/dev/null; then
    echo "bunkerd started"
else
    echo "bunkerd failed to start"
    exit 1
fi
