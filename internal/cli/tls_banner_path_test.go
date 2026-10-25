package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// REV-BUNKER-010: the TLS TOFU refusal banners used to hardcode
// ~/.bunker/config.yaml as the place to remove tls_insecure from. Under
// GAP-181 the CLI config file resolves through configFilePath() — the same
// resolver `bunker connect` writes through — so a BUNKER_HOME or --config
// deployment made the banner point at a file that is not the operator's
// config. These tests pin both banners (the RequireInsecureAck "pin wins"
// refusal and the validateEntryTLS contradictory-configuration refusal) to
// the path the resolver actually produces in each tier.
func TestTLSBannerNamesResolvedConfigPath(t *testing.T) {
	// An entry that trips BOTH banners: a pin plus a request to skip
	// verification is refused by RequireInsecureAck (called from the
	// transport builder) and by validateEntryTLS (the fail-closed check).
	pinned := ServerEntry{
		Name:        "pinned",
		URL:         "https://host:9090",
		TLSInsecure: true,
		CertPin:     gap141Pin,
	}

	assertBannerNamesPath := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil {
			t.Fatal("got nil error, want the pin-wins TLS refusal banner")
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("banner does not name the resolved config path %q:\n%s", want, err.Error())
		}
		if strings.Contains(err.Error(), "~/.bunker/config.yaml") {
			t.Errorf("banner still hardcodes the legacy path:\n%s", err.Error())
		}
	}

	t.Run("default resolution names the documented config dir", func(t *testing.T) {
		ResetConfigPathOverride()
		t.Cleanup(ResetConfigPathOverride)
		setenvForTest(t, envBunkerHome, "")
		setenvForTest(t, EnvBunkerConfigHome, "")

		// Post-GAP-181 the documented location is the per-user config dir
		// (${XDG_CONFIG_HOME:-~/.config}/bunker), not the legacy ~/.bunker —
		// this assertion fails against the old hardcoded banner.
		want := mustConfigFilePath()
		assertBannerNamesPath(t, RequireInsecureAck(pinned), want)
		assertBannerNamesPath(t, validateEntryTLS(pinned), want)
	})

	t.Run("BUNKER_HOME relocation is named", func(t *testing.T) {
		ResetConfigPathOverride()
		t.Cleanup(ResetConfigPathOverride)
		home := t.TempDir()
		setenvForTest(t, envBunkerHome, home)
		setenvForTest(t, EnvBunkerConfigHome, "")

		want := filepath.Join(home, "config.yaml")
		if got := mustConfigFilePath(); got != want {
			t.Fatalf("resolver = %q, want %q", got, want)
		}
		assertBannerNamesPath(t, RequireInsecureAck(pinned), want)
		assertBannerNamesPath(t, validateEntryTLS(pinned), want)
	})

	t.Run("the --config flag wins and is named", func(t *testing.T) {
		flagCfg := filepath.Join(t.TempDir(), "custom", "config.yaml")
		SetConfigPathOverride(flagCfg)
		t.Cleanup(ResetConfigPathOverride)

		if got := mustConfigFilePath(); got != flagCfg {
			t.Fatalf("resolver = %q, want the --config override %q", got, flagCfg)
		}
		assertBannerNamesPath(t, RequireInsecureAck(pinned), flagCfg)
		assertBannerNamesPath(t, validateEntryTLS(pinned), flagCfg)
	})
}
