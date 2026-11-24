#!/bin/sh
# One controlled transfer: create payload ONCE, hash it, cp it once, hash on agent, pull back, hash.
set -e
P=~/.hermes/cache/scratch/dogfood-bunker-1126/ctl
mkdir -p $P
head -c 30000000 /dev/urandom > $P/ctl.bin
echo "LOCAL_SRC_MD5  $(md5sum $P/ctl.bin)"
echo "LOCAL_SRC_SIZE $(stat -c %s $P/ctl.bin)"

echo "--- cp to agent ---"
/usr/bin/time -f 'CTL_CP_WALL %e s' bunker cp $P/ctl.bin c940c03f:/tmp/ctl.bin --server bunker-las-03
echo "CTL_CP_RC=$?"

echo "--- hash on agent ---"
bunker exec c940c03f --server bunker-las-03 -- sh -lc 'md5sum /tmp/ctl.bin; stat -c %s /tmp/ctl.bin'

echo "--- pull back ---"
/usr/bin/time -f 'CTL_PULL_WALL %e s' bunker pull c940c03f /tmp/ctl.bin $P/out --server bunker-las-03
echo "CTL_PULL_RC=$?"
echo "LOCAL_BACK_MD5 $(md5sum $P/out/ctl.bin)"
echo "LOCAL_BACK_SIZE $(stat -c %s $P/out/ctl.bin)"
