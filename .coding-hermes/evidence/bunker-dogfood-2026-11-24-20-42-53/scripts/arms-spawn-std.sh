#!/bin/sh
# Dogfood real-use arm part 1: spawn an agent and record the timing + stage marks.
# Scratch agent, short TTL, standard preset (the quickstart's own flags).
/usr/bin/time -f 'SPAWN_WALL %e s (wall) | user %U sys %S' \
  bunker spawn --cpu 2.0 --memory 2147483648 --ttl 50m --server bunker-las-03 2> ~/.hermes/cache/scratch/dogfood-bunker-1126/spawn.log
tail -40 ~/.hermes/cache/scratch/dogfood-bunker-1126/spawn.log
