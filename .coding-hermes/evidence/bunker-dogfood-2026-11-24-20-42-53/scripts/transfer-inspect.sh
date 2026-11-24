#!/bin/sh
# Inspect what actually landed after the cp failure, then attempt pull-back and hash.
P=~/.hermes/cache/scratch/dogfood-bunker-1126/ctl
echo "--- agent-side state of /tmp/ctl.bin ---"
bunker exec c940c03f --server bunker-las-03 -- sh -c 'md5sum /tmp/ctl.bin; stat -c %s /tmp/ctl.bin' 2>&1 | tail -3

echo "--- pull back ---"
/usr/bin/time -f 'CTL_PULL_WALL %e s' bunker pull c940c03f /tmp/ctl.bin $P/out --server bunker-las-03 2>&1
echo "CTL_PULL_RC=$?"
md5sum $P/out/ctl.bin 2>&1
stat -c %s $P/out/ctl.bin 2>&1
