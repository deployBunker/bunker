# INT-CI-091 local proto freshness probe

On the worker worktree, with a comment appended to the tracked proto and the
original stamp untouched (real `buf`, one attempt):

```text
$ PROTO_CACHE_HIT=true bash scripts/gitreins-guard.sh
proto: stale or missing proto/.codegen-fresh; run: bash .github/scripts/ensure-proto.sh && git add proto/.codegen-fresh
gitreins-guard: TEST-FAILURE (proto freshness)
gate_rc=1
$ PROTO_SKIP_MAX_ATTEMPTS=1 make proto
PROTO_STAMP_ENFORCE=0 bash .github/scripts/ensure-proto.sh
proto: inputs changed (stamp proto-gen-v1-0b7ee68602a9edf6795620f245548e830f11104bec9e8702f4039a17229c7070 != proto-gen-v1-10077b2968d4e3863084f154ee869e678ae92abd6dd1d9e3ce469efc3f2cc568) — regeneration needed
proto: buf generate attempt 1/1
proto: buf generate OK — stamped proto/.codegen-fresh = proto-gen-v1-10077b2968d4e3863084f154ee869e678ae92abd6dd1d9e3ce469efc3f2cc568
proto: NEEDED-and-DONE
make_rc=0
$ git status --porcelain -- proto/
 M proto/.codegen-fresh
 M proto/bunker/v1/bunker.proto
$ PROTO_STAMP_ENFORCE=1 bash .github/scripts/ensure-proto.sh
proto: generated code is current for inputs 10077b2968d4e3863084f154ee869e678ae92abd6dd1d9e3ce469efc3f2cc568 — skipping buf generate (no BSR call)
proto: SKIPPED
ci_step_rc=0
```

The probe inputs and stamp were restored afterward. The exact hosted step is
`.github/workflows/ci.yml`, build-and-test's “Generate protobuf code” step,
which runs `bash .github/scripts/ensure-proto.sh` at line 150. This local probe
is not a hosted CI run; hosted confirmation requires the foreman's next push.
The probe above predates the additional index-consistency check; the automated
fixture also covers partial staging and bypass attempts via `PROTO_CACHE_HIT`.
