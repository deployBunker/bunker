package cli

import (
	"crypto/tls"
	"net/http"
	"os"
	"time"

	"github.com/spf13/viper"

	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// newBunkerdClient creates a connect-go client for the given server entry.
// It applies TLS settings (including --tls-insecure) from the entry.
func newBunkerdClient(entry ServerEntry) bunkerv1connect.BunkerdClient {
	// 300s: spawn installs rootless docker inside the agent (can take minutes).
	// A 30s transport timeout made every slow spawn fail with deadline_exceeded
	// even though the spawn context allows 300s (observed 2026-08-27 on las-03).
	httpClient := &http.Client{Timeout: 300 * time.Second}
	if entry.TLSInsecure {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return bunkerv1connect.NewBunkerdClient(httpClient, entry.URL)
}

// resolveToken returns the auth token for a server entry, checking the entry,
// viper config, and BUNKER_TOKEN environment variable.
func resolveToken(entry ServerEntry) string {
	token := entry.Token
	if token == "" {
		token = viper.GetString("token")
	}
	if token == "" {
		token = os.Getenv("BUNKER_TOKEN")
	}
	return token
}
