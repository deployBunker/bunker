#!/usr/bin/env python3
"""DexDat Memory watchdog - check status and report."""
import os, json, subprocess, sys, glob

DATASETS_DIR = "/opt/dexdat-memory/testdata/datasets"
DONE_FILE = "/tmp/loader_done.txt"
LOADER_LOG = "/tmp/loader_out.log"

def run(cmd):
    result = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=30)
    return result.stdout.strip(), result.stderr.strip(), result.returncode

# 1. Count total dataset files (excluding .SKIPPED)
all_files = sorted([f for f in glob.glob(f"{DATASETS_DIR}/*.jsonl") if not f.endswith('.SKIPPED')])
total = len(all_files)

# 2. Read done files
done_files = set()
if os.path.exists(DONE_FILE):
    with open(DONE_FILE) as f:
        done_files = {line.strip() for line in f if line.strip()}

# 3. Find remaining files
remaining = [os.path.basename(f) for f in all_files if os.path.basename(f) not in done_files]
done_count = len(done_files)

print(f"=== DexDat Memory Watchdog Report ===")
print(f"Total dataset files: {total}")
print(f"Files processed:     {done_count}")
print(f"Files remaining:     {len(remaining)}")

# 4. Check DB record count via docker exec
db_out, db_err, db_rc = run("docker exec dexdat-postgres psql -U memory -d memorydb -t -c 'SELECT count(*) FROM memory_units' 2>/dev/null")
if db_rc == 0 and db_out.strip():
    try:
        count = int(db_out.strip())
        print(f"DB memory_units:     {count:,}")
    except:
        print(f"DB raw output:       {db_out.strip()[:200]}")
else:
    print(f"DB check:            FAILED (rc={db_rc}) - {db_err[:200]}")

# 5. Check memoryd health
mem_out, mem_err, mem_rc = run("curl -s http://localhost:8081/health 2>/dev/null")
if mem_rc == 0:
    print(f"memoryd health:      {mem_out[:200]}")
else:
    print(f"memoryd health:      unreachable")

# 6. Check loader process
pgrep_out, _, pgrep_rc = run("pgrep -f 'fast_ingest|loader' 2>/dev/null")
loader_running = pgrep_rc == 0 and bool(pgrep_out.strip())
print(f"Loader running:      {'YES' if loader_running else 'NO'}")

# 7. Recent loader log
if os.path.exists(LOADER_LOG):
    with open(LOADER_LOG) as f:
        lines = f.readlines()
        last_lines = lines[-15:] if len(lines) > 15 else lines
        print(f"\nLast {len(last_lines)} lines of loader log:")
        for line in last_lines:
            print(f"  {line.rstrip()[:200]}")

# 8. Print first 5 remaining files
if remaining:
    print(f"\nFirst 5 remaining files:")
    for f in remaining[:5]:
        print(f"  {f}")

# 9. Summary
print(f"\n=== Action Required ===")
if loader_running:
    print("Loader is running. No action needed.")
    print("[SILENT]" if remaining == [] else "")
else:
    if remaining:
        print(f"Loader is DEAD. {len(remaining)} files need processing. Starting loader...")
        print("ACTION: START_LOADER")
    else:
        print("Loader is dead but all files processed. No action needed.")
