#!/bin/bash
# QA-BUNKER-33 — regression test: the ui-probe Go-native gate must detect HTTP
# entrypoints in STANDARD Go layouts. Pre-fix the gate grepped ONLY root-level
# *.go (`grep -qE '…' *.go`), so a repo whose server lives under cmd/ (the
# standard layout: cmd/server/main.go + http.ListenAndServe) matched nothing
# and fell through to N/A "no web UI detected" — a false negative on the exact
# repos the arm's BIN detection already supports (go run ./cmd/<pkg> + the
# start-cmd fallback queue).
#
# Two layers, both extracting the SHIPPED bytes from bunker-qa.sh (no copy):
#   (1) the gate predicate itself, probed against fixture repos:
#       (a) cmd/ layout      -> DETECTED  (THE FIX)
#       (b) pkg/ layout      -> DETECTED
#       (c) internal/ layout -> DETECTED
#       (d) root main.go     -> DETECTED  (backward compatible)
#       (e) go repo, no server -> NOT detected (N/A stays correct)
#       (f) server idiom in vendor/ only -> NOT detected (exclusion)
#       (g) server idiom in .git/ only   -> NOT detected (exclusion)
#       (h) idiom in a non-.go file      -> NOT detected (--include guard)
#   (2) an agent-shaped E2E: extract the qa-run.sh BIN-detection snippet,
#       unescape \$ -> $ exactly as the `cat <<EOF` heredoc does, resolve BIN
#       for a cmd/ fixture (expect `go run ./cmd/server`), start it setsid+
#       timeout like the arm does, and poll /docs then / for `200 text/html`.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected [$2] got [$3]"; fail=$((fail+1)); fi
}

TMPD=${TMPDIR:-/tmp}/qa_bunker_33.$$
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT

# --- Layer 1: extract the shipped gate predicate ---------------------------
# The gate is a single `elif [ -f go.mod ] && grep ... ; then` line inside the
# qa-run.sh heredoc. Strip `elif ` and the trailing `; then` to get a
# standalone predicate (exit 0 = go-native arm ENTERS).
GATE_LINE=$(grep -n "elif \[ -f go.mod \] && grep -q.*'http" "$HARNESS" | head -1 | cut -d: -f1)
[ -n "$GATE_LINE" ] || { echo "FAIL: Go-native gate line not found in $HARNESS"; exit 1; }
sed -n "${GATE_LINE}p" "$HARNESS" | sed -e 's/^elif //' -e 's/; then$//' > "$TMPD/gate.sh"
grep -q '\[ -f go.mod \]' "$TMPD/gate.sh" || { echo "FAIL: gate extraction malformed"; exit 1; }
# The pre-fix line globbed *.go non-recursively; the fixed line must scan the
# tree and exclude dependency/metadata dirs (grep -q would NOT catch this —
# assert the flags explicitly so a revert of the glob is loud).
grep -q -- "-qrE" "$TMPD/gate.sh" || { echo "FAIL: gate lost recursive grep -qrE"; fail=$((fail+1)); }
grep -q -- "--include='\*.go'" "$TMPD/gate.sh" || { echo "FAIL: gate lost --include='*.go'"; fail=$((fail+1)); }
grep -q -- "--exclude-dir=.git" "$TMPD/gate.sh" || { echo "FAIL: gate lost --exclude-dir=.git"; fail=$((fail+1)); }

gate() { # <repo-dir> -> DETECTED | NOT_DETECTED
  ( cd "$1" && bash "$TMPD/gate.sh" >/dev/null 2>&1 && echo DETECTED || echo NOT_DETECTED )
}

# --- fixtures ---------------------------------------------------------------
gomod() { printf 'module example.com/fixture\n\ngo 1.21\n' > "$1/go.mod"; }

# (a) cmd/ layout — THE FIX: server entrypoint under cmd/server/.
mkdir -p "$TMPD/repos/cmdserver/cmd/server"
gomod "$TMPD/repos/cmdserver"
cat > "$TMPD/repos/cmdserver/cmd/server/main.go" <<'EOF'
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("FIXTURE_PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<html><body>fixture UI</body></html>")
	})
	http.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body>fixture docs</body></html>")
	})
	http.ListenAndServe("127.0.0.1:"+port, nil)
}
EOF
# a second, non-server cmd/ main that sorts AFTER cmd/server — the start-cmd
# fallback queue must append it behind the detected BIN (alphabetical-first
# BIN resolution would otherwise pick the CLI and miss the server).
mkdir -p "$TMPD/repos/cmdserver/cmd/zcli"
cat > "$TMPD/repos/cmdserver/cmd/zcli/main.go" <<'EOF'
package main

import "fmt"

func main() { fmt.Println("fixture cli, not a server") }
EOF

# (b) pkg/ layout.
mkdir -p "$TMPD/repos/pkgapi/pkg/api"
gomod "$TMPD/repos/pkgapi"
printf 'package api\n\nimport "net/http"\n\nfunc Routes() { http.ListenAndServe(":9000", nil) }\n' \
  > "$TMPD/repos/pkgapi/pkg/api/routes.go"

# (c) internal/ layout.
mkdir -p "$TMPD/repos/internalsvc/internal/svc"
gomod "$TMPD/repos/internalsvc"
printf 'package svc\n\nimport "github.com/go-chi/chi"\n\nfunc New() { chi.NewRouter() }\n' \
  > "$TMPD/repos/internalsvc/internal/svc/svc.go"

# (d) root main.go — backward compatibility.
mkdir -p "$TMPD/repos/rootmain"
gomod "$TMPD/repos/rootmain"
printf 'package main\n\nimport "net/http"\n\nfunc main() { http.ListenAndServe(":8000", nil) }\n' \
  > "$TMPD/repos/rootmain/main.go"

# (e) go repo with NO server — the N/A must stay correct (no false ENTER).
mkdir -p "$TMPD/repos/noserver/cmd/cli"
gomod "$TMPD/repos/noserver"
printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("plain cli") }\n' \
  > "$TMPD/repos/noserver/cmd/cli/main.go"

# (f) vendored server library — exclusion, not detection.
mkdir -p "$TMPD/repos/vendoronly/vendor/github.com/some/srv"
gomod "$TMPD/repos/vendoronly"
printf 'package srv\n\nimport "net/http"\n\nfunc S() { http.ListenAndServe(":7000", nil) }\n' \
  > "$TMPD/repos/vendoronly/vendor/github.com/some/srv/srv.go"

# (g) idiom only inside .git metadata — exclusion.
mkdir -p "$TMPD/repos/gitnoise/.git"
gomod "$TMPD/repos/gitnoise"
printf 'commit msg mentioning http.Server\n' > "$TMPD/repos/gitnoise/.git/COMMIT_EDITMSG"
printf 'package main\n\nfunc main() {}\n' > "$TMPD/repos/gitnoise/main.go"

# (h) idiom in a NON-Go file — --include='*.go' guard.
mkdir -p "$TMPD/repos/nonotgo/cmd/server"
gomod "$TMPD/repos/nonotgo"
printf '// http.ListenAndServe lives here\n' > "$TMPD/repos/nonotgo/cmd/server/notes.md"

echo "QA-BUNKER-33: ui-probe Go-native gate sees standard layouts"
echo
check "cmd/server/main.go (http.ListenAndServe) -> DETECTED"  "DETECTED"     "$(gate "$TMPD/repos/cmdserver")"
check "pkg/api (http.ListenAndServe) -> DETECTED"             "DETECTED"     "$(gate "$TMPD/repos/pkgapi")"
check "internal/svc (chi.NewRouter) -> DETECTED"              "DETECTED"     "$(gate "$TMPD/repos/internalsvc")"
check "root main.go (backward compat) -> DETECTED"            "DETECTED"     "$(gate "$TMPD/repos/rootmain")"
check "go repo with no server -> NOT detected (N/A correct)"  "NOT_DETECTED" "$(gate "$TMPD/repos/noserver")"
check "server idiom in vendor/ only -> NOT detected"          "NOT_DETECTED" "$(gate "$TMPD/repos/vendoronly")"
check "server idiom in .git/ only -> NOT detected"            "NOT_DETECTED" "$(gate "$TMPD/repos/gitnoise")"
check "idiom in non-.go file -> NOT detected"                 "NOT_DETECTED" "$(gate "$TMPD/repos/nonotgo")"

# --- Layer 2: agent-shaped E2E on the cmd/ fixture --------------------------
# Extract the qa-run.sh Go BIN-detection snippet (gofirst ladder) and unescape
# `\$` -> `$` exactly as the unquoted heredoc does when generating qa-run.sh.
BIN_TMP="$TMPD/bin"
mkdir -p "$BIN_TMP"
awk '
  /^  gofirst=""$/ {f=1}
  f {print}
  f && /^  fi$/ {exit}
' "$HARNESS" | sed 's/\\\$/$/g' > "$BIN_TMP/bindetect.sh"
grep -q 'gofirst' "$BIN_TMP/bindetect.sh" || { echo "FAIL: BIN-detection snippet not extracted"; exit 1; }
cat > "$BIN_TMP/probe.sh" <<'EOF'
#!/bin/bash
cd "$1" || exit 1
source "$2"
BIN=""
gofirst=""
if [ -d cmd ]; then
  gofirst=$(grep -rlsE '^package main' cmd --include='*.go' 2>/dev/null | sort | head -1)
fi
if [ -n "$gofirst" ]; then BIN="go run ./$(dirname "$gofirst")"
elif ls ./*.go >/dev/null 2>&1 && grep -qsE '^package main' ./*.go; then BIN="go run ."
fi
printf '%s\n' "${BIN:-UNSET}"
EOF
chmod +x "$BIN_TMP/probe.sh"

got=$("$BIN_TMP/probe.sh" "$TMPD/repos/cmdserver" "$BIN_TMP/bindetect.sh")
check "agent BIN detection: cmd/ fixture -> go run ./cmd/server" "go run ./cmd/server" "$got"

# Start the fixture exactly like the arm: setsid + timeout 45, stdin from
# /dev/null, log file — INSIDE the fixture dir (the arm runs at the repo root
# on the agent). Port: first FREE port from the arm's default UI_PORTS list
# (mirrors the arm's occupied-port baseline; keeps the test host-agnostic),
# handed to the fixture via FIXTURE_PORT. Then poll /docs then / for
# `200 text/html` (the arm's first-HTML-wins poll). Teardown: process-group
# kill then fuser sweep.
UI_PORT=""
for p in 3000 5173 8000 8080 8081 8766 8767 9000; do
  c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 "http://127.0.0.1:$p/" 2>/dev/null)
  if [ "$c" = "000" ]; then UI_PORT="$p"; break; fi
done
[ -n "$UI_PORT" ] || { echo "FAIL: no free port in the arm's default UI_PORTS list"; exit 1; }
LOG="$TMPD/ui-probe.log"
( cd "$TMPD/repos/cmdserver" && FIXTURE_PORT="$UI_PORT" setsid timeout 45 go run ./cmd/server ) </dev/null >"$LOG" 2>&1 &
UIPID=$!
UI_HIT=""
for i in $(seq 1 25); do
  kill -0 "$UIPID" 2>/dev/null || break
  UI_R=$(curl -s -o /dev/null -w '%{http_code} %{content_type}' --max-time 2 "http://127.0.0.1:$UI_PORT/docs" 2>/dev/null)
  case "$UI_R" in "200 text/html"*) UI_HIT="$UI_PORT/docs"; break ;; esac
  UI_R=$(curl -s -o /dev/null -w '%{http_code} %{content_type}' --max-time 2 "http://127.0.0.1:$UI_PORT/" 2>/dev/null)
  case "$UI_R" in "200 text/html"*) UI_HIT="$UI_PORT/"; break ;; esac
  sleep 1
done
check "E2E: go run ./cmd/server serves 200 text/html on :$UI_PORT/docs" "$UI_PORT/docs" "${UI_HIT:-none}"
UI_PGID=$(ps -o pgid= -p "$UIPID" 2>/dev/null | tr -d ' ')
if [ -n "$UI_PGID" ] && [ "$UI_PGID" = "$UIPID" ]; then
  kill -- -"$UIPID" 2>/dev/null || true; sleep 1; kill -9 -- -"$UIPID" 2>/dev/null || true
fi
kill "$UIPID" 2>/dev/null || true; sleep 1; kill -9 "$UIPID" 2>/dev/null || true
fuser -k "$UI_PORT"/tcp >/dev/null 2>&1 || true

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
