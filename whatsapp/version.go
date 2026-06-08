package whatsapp

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"whatsapp-mcp/config"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

// ConfigureWebVersion refreshes the WhatsApp Web version used by whatsmeow.
// WhatsApp rejects stale client revisions before sending a QR code.
func ConfigureWebVersion(ctx context.Context, logger *log.Logger) {
	if logger == nil {
		logger = log.Default()
	}

	if versionOverride := os.Getenv("WHATSAPP_WEB_VERSION"); versionOverride != "" {
		version, err := store.ParseVersion(versionOverride)
		if err != nil {
			logger.Printf("Warning: invalid WHATSAPP_WEB_VERSION %q: %v", versionOverride, err)
			return
		}
		setWAVersion(version)
		logger.Printf("Using WhatsApp Web version from WHATSAPP_WEB_VERSION: %s", version.String())
		return
	}

	if !config.GetEnvBool("WHATSAPP_WEB_VERSION_AUTO_UPDATE", true) {
		logger.Printf("WhatsApp Web version auto-update disabled; using bundled version %s", store.GetWAVersion().String())
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	latest, err := whatsmeow.GetLatestVersion(ctx, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		logger.Printf("Warning: failed to fetch latest WhatsApp Web version, using bundled version %s: %v", store.GetWAVersion().String(), err)
		return
	}

	setWAVersion(*latest)
	logger.Printf("Using latest WhatsApp Web version: %s", latest.String())
}

func setWAVersion(version store.WAVersionContainer) {
	store.SetWAVersion(version)
	if store.BaseClientPayload != nil && store.BaseClientPayload.UserAgent != nil {
		store.BaseClientPayload.UserAgent.AppVersion = version.ProtoAppVersion()
	}
}
