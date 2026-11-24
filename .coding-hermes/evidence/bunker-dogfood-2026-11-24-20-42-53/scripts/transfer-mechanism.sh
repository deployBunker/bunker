#!/bin/sh
# Truncation mechanism: what do the agent's own limits say, and where does a direct write stop?
bunker exec c940c03f --server bunker-las-03 -- sh -c '
echo ULIMIT_F=$(ulimit -f)
echo ULIMIT_F_BLOCKS_KB=$(ulimit -f)
head -c 30000000 /dev/urandom > /tmp/fsize-test.bin
echo DIRECT_WRITE_SIZE=$(stat -c %s /tmp/fsize-test.bin)
dd if=/dev/zero of=/tmp/fsize-test2.bin bs=1048576 count=40 status=none 2>/tmp/dd-err.txt
echo DD_SIZE=$(stat -c %s /tmp/fsize-test2.bin 2>/dev/null)
cat /tmp/dd-err.txt
head -c 60000000 /dev/urandom > /tmp/fsize-test3.bin
echo BIG_WRITE_SIZE=$(stat -c %s /tmp/fsize-test3.bin)
rm -f /tmp/fsize-test*.bin /tmp/fsize-test2.bin
' 2>&1 | tail -8
echo ===INFO_LIMITS===
bunker info c940c03f --server bunker-las-03 2>&1 | grep -iE 'fsize|disk|preset|file size'