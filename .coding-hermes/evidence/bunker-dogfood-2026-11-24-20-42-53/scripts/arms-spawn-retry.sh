#!/bin/sh
# A/B arm 3: retry the standard-preset spawn verbatim (transient race vs persistent refusal on las-03).
/usr/bin/time -f 'STD_RETRY_WALL %e s' bunker spawn --cpu 2.0 --memory 2147483648 --ttl 30m --server bunker-las-03 2>>~/.hermes/cache/scratch/dogfood-bunker-1126/std-retry.log
echo "RC=$?"
tail -12 ~/.hermes/cache/scratch/dogfood-bunker-1126/std-retry.log
