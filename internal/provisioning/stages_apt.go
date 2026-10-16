// Delivery stage scripts. Each stage is ONE POSIX sh program run on the agent
// through the audited exec RPC (the same path `bunker exec` uses), and each
// must end by printing its verdict line:
//
//	NAME\tINSTALLED\t<version the delivered binary reports | already-present>
//	NAME\tFAILED\t<stage that failed + the agent's own words>
//
// A stage that cannot PROVE its result is a FAILED stage — "copied the file"
// is not "the toolchain works". The gcc stage's proof is a real
// compile-link-run of a conftest program; the others require the delivered
// binary to answer its version query.
//
// Every path is single-quoted at build time (no agent-controlled input ever
// becomes shell syntax — the stages take no arguments), every extraction
// target is the agent's own HOME (no root), and a stage leaves a reusable
// cache directory behind so the second run is a no-op probe.
package provisioning

import "strings"

// quoteSh single-quotes s for POSIX sh — the same discipline internal/server
// and internal/cli apply to remote commands. Local copy so this package has
// no dependency direction into cli.
func quoteSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// stageHeader opens a stage: set +e (verdict lines, not exit codes, carry the
// outcome), NAME from the catalog entry, and the two directory variables.
func stageHeader(name string) string {
	var b strings.Builder
	b.WriteString("set +e\n")
	b.WriteString("name=" + quoteSh(name) + "\n")
	b.WriteString("b=\"$HOME/" + InstallDir + "\"\n")
	b.WriteString("root=\"$HOME/" + CacheRoot + "\"\n")
	b.WriteString("mkdir -p \"$b\" 2>/dev/null || { printf '%s\\tFAILED\\tcannot create %s\\n' \"$name\" \"$b\"; exit 0; }\n")
	return b.String()
}

// dlGoBase / nodeBase / rustupBase are the download origins, kept as
// constants so the pin tests can assert the full URL shape.
const dlGoBase = "https://dl.google.com/go/"
const nodeBase = "https://nodejs.org/dist/"
const rustupBase = "https://static.rust-lang.org/rustup/dist/"

// tarballBase exists so string concatenation of a base + version stays a
// single greppable literal per stage.
func tarballBase(base string) string { return base }

// goToolchainStage delivers the checksum-pinned Go release tarball into
// $HOME/.bunker/toolchains/go, then symlinks the go binary into $HOME/bin.
// Same pin internal/cli's gopls bootstrap uses (kept in sync by the pin
// tests). The installed toolchain persists as the cache; a re-run is a no-op.
func goToolchainStage() string {
	var b strings.Builder
	b.WriteString(stageHeader("go"))
	b.WriteString(`if [ -x "$root/go/bin/go" ]; then
  printf '%s\tINSTALLED\talready-present\n' "$name"; exit 0
fi
case "$(uname -m)" in
  x86_64) garch=amd64; gsha=` + GoToolchainSHA256AMD64 + ` ;;
  aarch64|arm64) garch=arm64; gsha=` + GoToolchainSHA256ARM64 + ` ;;
  *) printf '%s\tFAILED\tunsupported architecture %s\n' "$name" "$(uname -m)"; exit 1 ;;
esac
tmp=$(mktemp -d 2>/dev/null) || { printf '%s\tFAILED\tmktemp failed\n' "$name"; exit 0; }
(
  cd "$tmp" || exit 1
  log="$tmp/go.log"
  curl -fsSL "` + tarballBase(dlGoBase) + GoToolchainVersion + `.linux-$garch.tar.gz" -o go.tgz >>"$log" 2>&1 || {
    printf '%s\tFAILED\ttoolchain download: %s\n' "$name" "stage-failed"; exit 1
  }
  echo "$gsha  go.tgz" | sha256sum -c - >>"$log" 2>&1 || {
    printf '%s\tFAILED\ttoolchain checksum mismatch (expected %s for %s)\n' "$name" "$gsha" "$garch"; exit 1
  }
  tar -xzf go.tgz >>"$log" 2>&1 || {
    printf '%s\tFAILED\ttoolchain extract: %s\n' "$name" "stage-failed"; exit 1
  }
  mkdir -p "$root"
  rm -rf "$root/go.tmp"
  mv go "$root/go.tmp" >>"$log" 2>&1 || {
    last=$(tail -n 1 "$log" 2>/dev/null | cut -c1-100)
    printf '%s\tFAILED\tmove into %s: %s\n' "$name" "$root" "$last"; exit 1
  }
  mv "$root/go.tmp" "$root/go" >>"$log" 2>&1 || {
    printf '%s\tFAILED\tfinalize in %s: %s\n' "$name" "$root" "stage-failed"; exit 1
  }
  ln -sf "$root/go/bin/go" "$b/go" 2>>"$log"
)
rc=$?
rm -rf "$tmp"
[ $rc = 0 ] || exit $rc
# The verdict check is HERMETIC: GOTOOLCHAIN=local so the freshly installed
# toolchain never tries to auto-download a newer one (a go.mod with a higher
# go/toolchain line in the CWD would send "go version" off to fetch a
# different toolchain -- measured live), and cwd=/ so no go.mod is in play.
v=$(cd / && GOTOOLCHAIN=local "$b/go" version 2>/dev/null | awk 'NR==1 {print; exit}')
[ -n "$v" ] || { printf '%s\tFAILED\tinstalled but %s/go does not answer version\n' "$name" "$b"; exit 1; }
printf '%s\tINSTALLED\t%s\n' "$name" "$v"
exit 0
`)
	return b.String()
}

// gccToolchainStage delivers the C toolchain family from the apt archive
// WITHOUT root. The dependency closure of the release's own gcc package is
// resolved by apt ITSELF (apt-cache depends --recurse on the candidate's
// versioned name) — no release→package-version table is hardcoded, so the
// same stage is correct on every allowed base. dpkg-deb -x unpacks the
// signature-verified .deb files into the agent's cache; the $HOME/bin entry
// is a wrapper the stage WRITES FROM THE EXTRACTED LAYOUT it discovers (the
// driver's cc1/as/ld and startfiles live at layout-relative paths, which is
// also why the wrapper pins them with -B). The verdict is a
// compile-link-run proof: if any piece of the family failed to line up, the
// compiler's own words are the failure detail.
func gccToolchainStage() string { return aptFamilyStage("gcc", "family") }

// makeToolchainStage delivers apt `make` (single-binary extract; the ripgrep
// shape) into $HOME/bin.
func makeToolchainStage() string { return aptFamilyStage("make", "single") }

// aptFamilyStage builds the apt-archive extraction stages. mode "family"
// assembles the gcc toolchain closure and proves it with compile-link-run;
// mode "single" extracts one package whose binary lands at usr/bin/<name>.
//
// apt list staleness: /var/lib/apt/lists is host-root-owned and typically
// EMPTY inside a fresh agent, and apt-cache/apt-get download read their
// candidates FROM those lists — a missing entry fails with "Unable to locate
// package" even though the archive has it. The stage refreshes the lists
// first when they are absent (best-effort: it may fail without root; the
// resolution attempt after it is the verdict).
func aptFamilyStage(name, mode string) string {
	var b strings.Builder
	b.WriteString(stageHeader(name))
	if mode == "family" {
		b.WriteString(`if [ -x "$root/gcc/usr/bin/"gcc-* ] 2>/dev/null && ls "$root"/.gcc-proof-ok >/dev/null 2>&1; then
  printf '%s\tINSTALLED\talready-present\n' "$name"; exit 0
fi
`)
	} else {
		b.WriteString(`if [ -x "$b/` + name + `" ]; then
  printf '%s\tINSTALLED\talready-present\n' "$name"; exit 0
fi
`)
	}
	b.WriteString(`if command -v apt-get >/dev/null 2>&1; then
  fetcher=apt-get
elif command -v apt >/dev/null 2>&1; then
  fetcher=apt
else
  printf '%s\tFAILED\tno apt-get or apt on the agent\n' "$name"; exit 0
fi
lists=/var/lib/apt/lists
[ -n "$(ls -A "$lists" 2>/dev/null | head -n 1)" ] || "$fetcher" update >/dev/null 2>&1 || true
`)
	if mode == "single" {
		b.WriteString(`tmp=$(mktemp -d 2>/dev/null) || { printf '%s\tFAILED\tmktemp failed\n' "$name"; exit 0; }
(cd "$tmp" || exit 1
  log="$tmp/apt.log"
  "$fetcher" download ` + name + ` >>"$log" 2>&1 || {
    printf '%s\tFAILED\t%s download: %s\n' "$name" "$fetcher" "stage-failed"; exit 1
  }
  extracted=0
  for d in ./*.deb; do
    [ -e "$d" ] || continue
    dpkg-deb -x "$d" "$tmp/x" >>"$log" 2>&1 || {
      printf '%s\tFAILED\tdpkg-deb -x: %s\n' "$name" "stage-failed"; exit 1
    }
    extracted=1
  done
  [ "$extracted" = 1 ] || { printf '%s\tFAILED\t%s download produced no .deb\n' "$name" "$fetcher"; exit 1; }
  bin="$tmp/x/usr/bin/` + name + `"
  [ -f "$bin" ] || { printf '%s\tFAILED\tunexpected deb layout: no usr/bin/%s\n' "$name" "$name"; exit 1; }
  cp "$bin" "$b/` + name + `.new" >>"$log" 2>&1 || {
    printf '%s\tFAILED\tcopy into %s: %s\n' "$name" "$b" "stage-failed"; exit 1
  }
  chmod 0755 "$b/` + name + `.new" 2>>"$log"
  mv -f "$b/` + name + `.new" "$b/` + name + `" 2>>"$log" || {
    printf '%s\tFAILED\tfinalize in %s: %s\n' "$name" "$b" "stage-failed"; exit 1
  }
)
rc=$?
rm -rf "$tmp"
[ $rc = 0 ] || exit $rc
v=$("$b/` + name + `" --version 2>/dev/null | awk 'NR==1 {print; exit}')
[ -n "$v" ] || { printf '%s\tFAILED\tinstalled but %s/` + name + ` does not answer --version\n' "$name" "$b"; exit 1; }
printf '%s\tINSTALLED\t%s\n' "$name" "$v"
exit 0
`)
		return b.String()
	}
	// "family" mode: the gcc toolchain closure.
	b.WriteString(`tmp=$(mktemp -d 2>/dev/null) || { printf '%s\tFAILED\tmktemp failed\n' "$name"; exit 0; }
(cd "$tmp" || exit 1
  log="$tmp/apt.log"
  cand=$(apt-cache policy gcc 2>/dev/null | awk '/Candidate:/ {print $2; exit}')
  case "$cand" in
    [0-9]*.[0-9]*) gver=${cand%%.*}; gver=${gver##*:} ;;
    *) printf '%s\tFAILED\tapt-cache has no gcc candidate (lists empty and update failed)\n' "$name"; exit 1 ;;
  esac
  case "$gver" in
    [0-9][0-9]) ;;
    [0-9]) ;;
    *) printf '%s\tFAILED\tgcc candidate %s parsed to a non-version %s\n' "$name" "$cand" "$gver"; exit 1 ;;
  esac
  pkgs=$(apt-cache depends --recurse --no-recommends --no-suggests --no-conflicts \
        --no-breaks --no-replaces --no-enhances "gcc-$gver" 2>/dev/null \
        | grep -E '^[a-zA-Z0-9]' | grep -v '<' | sort -u)
  [ -n "$pkgs" ] || { printf '%s\tFAILED\tapt-cache resolved no dependency closure for gcc-%s\n' "$name" "$gver"; exit 1; }
  # shellcheck disable=SC2086
  "$fetcher" download $pkgs >>"$log" 2>&1 || {
    last=$(tail -n 1 "$log" 2>/dev/null | cut -c1-100)
    printf '%s\tFAILED\t%s download of the gcc-%s closure: %s\n' "$name" "$fetcher" "$gver" "$last"; exit 1
  }
  mkdir -p "$root/gcc"
  for d in ./*.deb; do
    [ -e "$d" ] || continue
    dpkg-deb -x "$d" "$root/gcc" >>"$log" 2>&1 || {
      printf '%s\tFAILED\tdpkg-deb -x %s: %s\n' "$name" "$d" "stage-failed"; exit 1
    }
  done
  gccbin=$(ls "$root"/gcc/usr/bin/gcc-"$gver" 2>/dev/null | head -n 1)
  [ -n "$gccbin" ] || { printf '%s\tFAILED\textracted layout has no usr/bin/gcc-%s\n' "$name" "$gver"; exit 1; }
  gccdir=$(ls -d "$root"/gcc/usr/lib/gcc/*-linux-gnu/"$gver" 2>/dev/null | head -n 1)
  [ -n "$gccdir" ] || { printf '%s\tFAILED\textracted layout has no usr/lib/gcc/*-linux-gnu/%s\n' "$name" "$gver"; exit 1; }
  triple=$(basename "$(dirname "$gccdir")")
  {
    printf '#!/bin/sh\n'
    printf '# Bunker SURF-009: extracted (not installed) gcc-%s family.\n' "$gver"
    printf 'root="$HOME/%s/gcc"\n' '` + CacheRoot + `'
    printf 'g="%s" d="%s" t="%s"\n' "$gccbin" "$gccdir" "$triple"
    printf 'exec "$g" -B"$d" -B"$root/usr/bin" -B"$root/usr/lib/$t" -isystem "$d/include" -isystem "$root/usr/include/$t" -isystem "$root/usr/include" -L"$d" -L"$root/usr/lib/$t" "$@"\n'
  } >"$b/.gcc-wrapper.tmp"
  chmod 0755 "$b/.gcc-wrapper.tmp" && mv -f "$b/.gcc-wrapper.tmp" "$b/gcc" || {
    printf '%s\tFAILED\twrite wrapper into %s\n' "$name" "$b"; exit 1
  }
  printf 'int main(void){return 0;}\n' > conf.c
  if ! "$b/gcc" -o conf conf.c 2>"conf.err"; then
    last=$(tail -n 1 conf.err 2>/dev/null | cut -c1-100)
    printf '%s\tFAILED\tcompile proof: %s\n' "$name" "$last"; exit 1
  fi
  ./conf || { printf '%s\tFAILED\tcompile proof: linked binary exits %s\n' "$name" "$?"; exit 1; }
  printf '#include <stdio.h>\nint main(void){printf("ok\\n");return 0;}\n' > conf2.c
  if ! "$b/gcc" -o conf2 conf2.c 2>"conf2.err"; then
    last=$(tail -n 1 conf2.err 2>/dev/null | cut -c1-100)
    printf '%s\tFAILED\theader/link proof: %s\n' "$name" "$last"; exit 1
  fi
  out=$(./conf2 2>/dev/null)
  [ "$out" = "ok" ] || { printf '%s\tFAILED\theader/link proof: unexpected output %s\n' "$name" "$out"; exit 1; }
  touch "$root/.gcc-proof-ok"
)
rc=$?
rm -rf "$tmp"
[ $rc = 0 ] || exit $rc
v=$("$b/gcc" --version 2>/dev/null | awk 'NR==1 {print; exit}')
[ -n "$v" ] || { printf '%s\tFAILED\twrapper wrote but %s/gcc does not answer --version\n' "$name" "$b"; exit 1; }
printf '%s\tINSTALLED\t%s\n' "$name" "$v"
exit 0
`)
	return b.String()
}
