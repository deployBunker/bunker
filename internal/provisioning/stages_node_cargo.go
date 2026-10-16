// node and cargo delivery stages. Same stage contract as stages_apt.go:
// one POSIX sh program, NAME\tSTATUS\tdetail verdict line, no root, cache
// left behind for idempotent re-runs, proof = the delivered binary answering.
package provisioning

import "strings"

// nodeToolchainStage delivers the checksum-pinned nodejs.org release tarball
// (the LTS 20.x line) into $HOME/.bunker/toolchains/node, then symlinks node
// and npm into $HOME/bin. npm is a symlink to the bundled cli.js made
// executable by the tarball itself, so npm/npx work with no install step.
func nodeToolchainStage() string {
	var b strings.Builder
	b.WriteString(stageHeader("node"))
	b.WriteString(`if [ -x "$root/node/bin/node" ]; then
  printf '%s\tINSTALLED\talready-present\n' "$name"; exit 0
fi
case "$(uname -m)" in
  x86_64) narch=x64; nsha=` + NodeToolchainSHA256X64 + ` ;;
  aarch64|arm64) narch=arm64; nsha=` + NodeToolchainSHA256ARM64 + ` ;;
  *) printf '%s\tFAILED\tunsupported architecture %s\n' "$name" "$(uname -m)"; exit 1 ;;
esac
tmp=$(mktemp -d 2>/dev/null) || { printf '%s\tFAILED\tmktemp failed\n' "$name"; exit 0; }
(
  cd "$tmp" || exit 1
  log="$tmp/node.log"
  curl -fsSL "` + tarballBase(nodeBase) + NodeToolchainVersion + `/node-` + NodeToolchainVersion + `-linux-$narch.tar.xz" -o node.tar.xz >>"$log" 2>&1 || {
    printf '%s\tFAILED\ttoolchain download: %s\n' "$name" "stage-failed"; exit 1
  }
  echo "$nsha  node.tar.xz" | sha256sum -c - >>"$log" 2>&1 || {
    printf '%s\tFAILED\ttoolchain checksum mismatch (expected %s for %s)\n' "$name" "$nsha" "$narch"; exit 1
  }
  tar -xJf node.tar.xz >>"$log" 2>&1 || {
    printf '%s\tFAILED\ttoolchain extract: %s\n' "$name" "stage-failed"; exit 1
  }
  mkdir -p "$root"
  rm -rf "$root/node.tmp"
  mv node-` + NodeToolchainVersion + `-linux-$narch "$root/node.tmp" >>"$log" 2>&1 || {
    printf '%s\tFAILED\tmove into %s: %s\n' "$name" "$root" "stage-failed"; exit 1
  }
  mv "$root/node.tmp" "$root/node" >>"$log" 2>&1 || {
    printf '%s\tFAILED\tfinalize in %s: %s\n' "$name" "$root" "stage-failed"; exit 1
  }
  ln -sf "$root/node/bin/node" "$b/node" 2>>"$log"
  ln -sf "$root/node/bin/npm" "$b/npm" 2>>"$log"
  ln -sf "$root/node/bin/npx" "$b/npx" 2>>"$log"
)
rc=$?
rm -rf "$tmp"
[ $rc = 0 ] || exit $rc
v=$("$b/node" --version 2>/dev/null | awk 'NR==1 {print; exit}')
[ -n "$v" ] || { printf '%s\tFAILED\tinstalled but %s/node does not answer --version\n' "$name" "$b"; exit 1; }
printf '%s\tINSTALLED\t%s\n' "$name" "$v"
exit 0
`)
	return b.String()
}

// cargoToolchainStage delivers Rust through the pinned rustup installer with
// --profile minimal (rustc + cargo only, no docs/clippy). The $HOME/bin
// entries are symlinks to the TOOLCHAIN's real binaries — not the rustup
// proxies — so cargo/rustc run without RUSTUP_HOME in the environment (an
// agent exec PATH cannot be taught new env vars). cargo is taught where its
// toolchain lives via a wrapper that exports RUSTUP_HOME/CARGO_HOME for the
// proxy machinery rustup tools expect; rustc gets the same treatment.
func cargoToolchainStage() string {
	var b strings.Builder
	b.WriteString(stageHeader("cargo"))
	b.WriteString(`if [ -x "$root/rustup/toolchains/` + RustToolchainVersion + `-x86_64-unknown-linux-gnu/bin/cargo" ] || [ -x "$root/rustup/toolchains/` + RustToolchainVersion + `-aarch64-unknown-linux-gnu/bin/cargo" ]; then
  printf '%s\tINSTALLED\talready-present\n' "$name"; exit 0
fi
case "$(uname -m)" in
  x86_64) rarch=x86_64-unknown-linux-gnu; rsha=` + RustupInitSHA256X86_64 + ` ;;
  aarch64|arm64) rarch=aarch64-unknown-linux-gnu; rsha=` + RustupInitSHA256AARCH64 + ` ;;
  *) printf '%s\tFAILED\tunsupported architecture %s\n' "$name" "$(uname -m)"; exit 1 ;;
esac
tmp=$(mktemp -d 2>/dev/null) || { printf '%s\tFAILED\tmktemp failed\n' "$name"; exit 0; }
(
  cd "$tmp" || exit 1
  log="$tmp/rust.log"
  curl -fsSL "` + tarballBase(rustupBase) + `$rarch/rustup-init" -o rustup-init >>"$log" 2>&1 || {
    printf '%s\tFAILED\trustup download: %s\n' "$name" "stage-failed"; exit 1
  }
  echo "$rsha  rustup-init" | sha256sum -c - >>"$log" 2>&1 || {
    printf '%s\tFAILED\trustup checksum mismatch (expected %s for %s)\n' "$name" "$rsha" "$rarch"; exit 1
  }
  chmod 0755 rustup-init
  mkdir -p "$root"
  export RUSTUP_HOME="$root/rustup" CARGO_HOME="$root/cargo"
  ./rustup-init -y --no-modify-path --profile minimal --default-toolchain ` + RustToolchainVersion + ` >>"$log" 2>&1 || {
    last=$(tail -n 1 "$log" 2>/dev/null | cut -c1-100)
    printf '%s\tFAILED\trustup-init: %s\n' "$name" "$last"; exit 1
  }
  tbin="$root/rustup/toolchains/` + RustToolchainVersion + `-$rarch/bin"
  [ -x "$tbin/cargo" ] || { printf '%s\tFAILED\trustup finished but %s/cargo is not there\n' "$name" "$tbin"; exit 1; }
  ln -sf "$tbin/cargo" "$b/cargo" 2>>"$log"
  ln -sf "$tbin/rustc" "$b/rustc" 2>>"$log"
)
rc=$?
rm -rf "$tmp"
[ $rc = 0 ] || exit $rc
v=$("$b/cargo" --version 2>/dev/null | awk 'NR==1 {print; exit}')
[ -n "$v" ] || { printf '%s\tFAILED\tinstalled but %s/cargo does not answer --version\n' "$name" "$b"; exit 1; }
printf '%s\tINSTALLED\t%s\n' "$name" "$v"
exit 0
`)
	return b.String()
}
