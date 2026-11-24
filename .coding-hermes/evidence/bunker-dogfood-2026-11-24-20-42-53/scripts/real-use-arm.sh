#!/bin/sh
# Real-use arm on scratch agent c940c03f: cp in/out + exec + docker run, with timings.
# Perf-020-validated surface (bunker cp) and the quickstart's docker promise.
set -x
mkdir -p ~/.hermes/cache/scratch/dogfood-bunker-1126/payload
printf 'hello dogfood 2026-11-24\n' > ~/.hermes/cache/scratch/dogfood-bunker-1126/payload/tiny.txt
dd if=/dev/urandom of=~/.hermes/cache/scratch/dogfood-bunker-1126/payload/big.bin bs=1M count=60 status=none

A=c940c03f
S=bunker-las-03
P=~/.hermes/cache/scratch/dogfood-bunker-1126

echo '--- exec roundtrip (documented verb) ---'
/usr/bin/time -f 'EXEC_WALL %e s' bunker exec $A --server $S -- sh -lc 'echo OK-from-agent; uname -a' 2>&1 | tail -3

echo '--- cp tiny COLD (60B file, first transfer over fresh key) ---'
/usr/bin/time -f 'CP_TINY_COLD %e s' bunker cp $P/payload/tiny.txt $A:/tmp/dogfood-tiny.txt --server $S 2>&1 | tail -2

echo '--- cp tiny WARM (same file second time) ---'
/usr/bin/time -f 'CP_TINY_WARM %e s' bunker cp $P/payload/tiny.txt $A:/tmp/dogfood-tiny-warm.txt --server $S 2>&1 | tail -2

echo '--- cp 60MB COLD ---'
/usr/bin/time -f 'CP_60MB_COLD %e s' bunker cp $P/payload/big.bin $A:/tmp/dogfood-big.bin --server $S 2>&1 | tail -2

echo '--- cp 60MB WARM ---'
/usr/bin/time -f 'CP_60MB_WARM %e s' bunker cp $P/payload/big.bin $A:/tmp/dogfood-big-warm.bin --server $S 2>&1 | tail -2

echo '--- pull 60MB back out ---'
/usr/bin/time -f 'PULL_60MB %e s' bunker pull $A:/tmp/dogfood-big.bin $P/payload/big-back.bin --server $S 2>&1 | tail -2

echo '--- verify payload integrity ---'
md5sum $P/payload/big.bin $P/payload/big-back.bin 2>&1

echo '--- quickstart docker promise ---'
/usr/bin/time -f 'DOCKER_RUN_WALL %e s' bunker exec $A --server $S -- docker run --rm alpine echo hello 2>&1 | tail -3

echo '--- byte-identity check of tiny file on agent ---'
bunker exec $A --server $S -- sh -lc 'cat /tmp/dogfood-tiny.txt; ls -la /tmp/dogfood-*.bin' 2>&1 | tail -5
