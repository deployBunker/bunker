#!/bin/sh
# A/B arm 2: spawn with --preset open (is the swap-bar refusal standard-preset-only?).
/usr/bin/time -f 'OPEN_WALL %e s' bunker spawn --preset open --cpu 2.0 --memory 2147483648 --ttl 30m --server bunker-las-03 2>>~/.hermes/cache/scratch/dogfood-bunker-1126/open-spawn.log
echo "RC=$?"
cat ~/.hermes/cache/scratch/dogfood-bunker-1126/open-spawn.log | tail -30
