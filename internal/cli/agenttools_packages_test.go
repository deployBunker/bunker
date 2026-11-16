package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoDeliverScriptEmbedsAMD64ChecksumAsLiteral(t *testing.T) {
	script := goDeliverScript("gopls", "golang.org/x/tools/gopls@latest")()

	if !strings.Contains(script, "gsha="+goBootstrapSHA256AMD64) {
		t.Fatalf("rendered gopls script does not carry the amd64 checksum as a literal:\n%s", script)
	}
	if strings.Contains(script, "gsha=`") {
		t.Fatal("rendered gopls script performs command substitution immediately after gsha=")
	}
	if strings.Contains(script, "`"+goBootstrapSHA256AMD64) {
		t.Fatal("rendered gopls script wraps the amd64 checksum in backticks")
	}
}

func TestGoDeliverScriptChecksumFailureIncludesExpectedAndActual(t *testing.T) {
	const payload = "not the Go archive\n"

	home := t.TempDir()
	stub := filepath.Join(home, "stub-bin")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mkdir", "mktemp", "sha256sum", "awk", "rm"} {
		if err := os.Symlink("/usr/bin/"+name, filepath.Join(stub, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(stub, "uname"), "#!/bin/sh\nprintf '%s\\n' x86_64\n")
	writeExecutable(t, filepath.Join(stub, "curl"), "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"-o\" ]; then\n    shift\n    printf '%s' '"+payload+"' > \"$1\"\n  fi\n  shift\ndone\n")

	cmd := exec.Command("sh", "-c", goDeliverScript("gopls", "golang.org/x/tools/gopls@latest")())
	cmd.Env = []string{"HOME=" + home, "PATH=" + stub}
	outBytes, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("checksum mismatch must make the delivery script fail")
	}
	out := string(outBytes)
	actual := sha256.Sum256([]byte(payload))
	want := "toolchain checksum mismatch (expected " + goBootstrapSHA256AMD64 +
		" for amd64; actual " + hex.EncodeToString(actual[:]) + ")"
	if !strings.Contains(out, "gopls\tFAILED\t"+want) {
		t.Fatalf("checksum failure did not include expected and actual digests; output=%q, want substring=%q", out, want)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
