#!/usr/bin/env bash
# THE DECISIVE PROOF, on the CORRECT host this time (resolved from the agent
# record, not assumed from the client config).
#
# Question: does the agent's own systemd start toolsd as the agent user, and
# does SSH hand the client that socket — with no daemon process management?
set -u
NEW=/tmp/bunker-new
AGENT=sock-proof
SRV=bunker-las-02
DIST=/home/kara/coding-hermes-tools/dist/toolsd-linux-amd64
LOCAL_SOCK=/tmp/toolsd-local-$$.sock

cd /home/kara/bunker || exit 9
go build -o "$NEW" ./cmd/bunker || exit 1
"$NEW" destroy "$AGENT" --server "$SRV" >/dev/null 2>&1
"$NEW" spawn "$AGENT" --server "$SRV" --ttl 45m >/dev/null 2>&1
"$NEW" agent-tools "$AGENT" --server "$SRV" --install --binary "$DIST" 2>&1 | grep -E "^delivered|FAIL"

# Resolve the REAL host from the agent record (this is what the earlier runs
# got wrong: the client config's IP was a different machine).
HOST=$(ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 root@100.116.99.35 "hostname" 2>/dev/null | tail -1)
if [ -z "$HOST" ]; then
  HOST_IP=$(ssh -o StrictHostKeyChecking=no root@100.116.99.35 'hostname -I | cut -d" " -f2' 2>/dev/null | tail -1)
fi
echo "=== resolved agent host: 100.116.99.35 ($HOST) ==="

echo
echo "=== 1. install units THROUGH the agent's context ==="
"$NEW" exec "$AGENT" --server "$SRV" -- sh -c '
set -e
mkdir -p "$HOME/.config/systemd/user"
cat > "$HOME/.config/systemd/user/toolsd.socket" <<EOF
[Unit]
Description=toolsd socket (per-connection activation)
[Socket]
ListenStream=%t/toolsd.sock
SocketMode=0600
Accept=yes
[Install]
WantedBy=sockets.target
EOF
cat > "$HOME/.config/systemd/user/toolsd@.service" <<EOF
[Unit]
Description=toolsd MCP (one process per connection, as the agent user)
[Service]
ExecStart=%h/bin/toolsd mcp
StandardInput=socket
StandardOutput=socket
EOF
echo "  units installed in $HOME/.config/systemd/user"
' 2>&1 | tail -2

echo
echo "=== 2. the AGENT S OWN systemd activates the socket ==="
"$NEW" exec "$AGENT" --server "$SRV" -- sh -c '
  systemctl --user daemon-reload
  systemctl --user start toolsd.socket
  echo "  socket unit : $(systemctl --user is-active toolsd.socket)"
  echo "  socket path : /run/user/$(id -u)/toolsd.sock"
  echo "  listener    : $(stat -c "%U %a" /run/user/$(id -u)/toolsd.sock 2>/dev/null)"
  echo "  processes   : $(pgrep -c toolsd 2>/dev/null || echo 0) (0 = connection-activated)"
' 2>&1 | tail -5

echo
echo "=== 3. the socket AS THE AGENT HOST SEES IT ==="
UID_N=$("$NEW" exec "$AGENT" --server "$SRV" -- id -u 2>/dev/null | tr -d '\r' | tail -1)
AUSER=$("$NEW" exec "$AGENT" --server "$SRV" -- id -un 2>/dev/null | tr -d '\r' | tail -1)
KEY=/home/kara/.bunker/keys/$AGENT
echo "  agent user=$AUSER uid=$UID_N key=$KEY"
ssh -i "$KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    "$AUSER@100.116.99.35" "ls -l /run/user/$UID_N/toolsd.sock; echo '  hostname:' \$(hostname)" 2>&1 | tail -3

echo
echo "=== 4. SSH hands the client the remote socket (agent key, agent identity) ==="
rm -f "$LOCAL_SOCK"
ssh -i "$KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    -o ExitOnForwardFailure=yes \
    -N -L "$LOCAL_SOCK:/run/user/$UID_N/toolsd.sock" "$AUSER@100.116.99.35" &
SSH_PID=$!
sleep 3
ls -l "$LOCAL_SOCK" >/dev/null 2>&1 && echo "  local socket created (mode $(stat -c %a "$LOCAL_SOCK"))" || echo "  FORWARD FAILED"

echo
echo "=== 5. REAL JSON-RPC + a REAL file op over that socket ==="
python3 - "$LOCAL_SOCK" <<'PY'
import json, socket, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); s.settimeout(25)
s.connect(sys.argv[1])
def rpc(o):
    s.sendall((json.dumps(o)+"\n").encode()); b=b""
    while b"\n" not in b:
        c=s.recv(65536)
        if not c: break
        b+=c
    return json.loads(b.split(b"\n")[0])
init = rpc({"jsonrpc":"2.0","id":1,"method":"initialize","params":{
  "protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"bunker","version":"1"}}})
print("  initialize ->", json.dumps(init.get("result",{}).get("serverInfo", init))[:90])
t = rpc({"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}})
names=[x["name"] for x in t.get("result",{}).get("tools",[])]
print(f"  tools/list -> {len(names)} tools")
print("    basic   :", [n for n in names if n in ("toolsd_read","toolsd_write","toolsd_list")])
print("    advanced:", [n for n in names if n in ("toolsd_patch","toolsd_apply","toolsd_diff3","toolsd_lease")])
w = rpc({"jsonrpc":"2.0","id":3,"method":"tools/call","params":{
  "name":"toolsd_write","arguments":{"root":"/tmp","path":"sock-proof.txt","content":"through the socket\n"}}})
r = rpc({"jsonrpc":"2.0","id":4,"method":"tools/call","params":{
  "name":"toolsd_read","arguments":{"root":"/tmp","path":"sock-proof.txt"}}})
txt="".join(c.get("text","") for c in r.get("result",{}).get("content",[]))
print("  write+read over the socket ->", repr(txt[:50]))
print("  write isError:", w.get("result",{}).get("isError"))
s.close()
PY
RC=$?

echo
echo "=== 6. per-connection processes, owned by the AGENT ==="
"$NEW" exec "$AGENT" --server "$SRV" -- sh -c 'pgrep -a toolsd | head -2; echo "  count: $(pgrep -c toolsd 2>/dev/null || echo 0)"' 2>&1 | tail -3

echo
echo "=== cleanup ==="
kill $SSH_PID 2>/dev/null; rm -f "$LOCAL_SOCK"
ssh -i /home/kara/.bunker/keys/$AGENT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "bunker-$AGENT@100.116.99.35" "rm -f /tmp/sock-proof.txt" 2>/dev/null
"$NEW" destroy "$AGENT" --server "$SRV" 2>&1 | tail -2
echo "rpc_rc=$RC"
