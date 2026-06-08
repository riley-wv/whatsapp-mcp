// WhatsApp MCP Server provides isolated WhatsApp MCP instances over HTTP.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"whatsapp-mcp/tenant"

	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found, using environment variables only")
	}

	httpPort := getenv("MCP_PORT", "8080")
	logLevel := getenv("LOG_LEVEL", "INFO")
	timezoneName := getenv("TIMEZONE", "UTC")
	timezone, err := time.LoadLocation(timezoneName)
	if err != nil {
		log.Printf("Warning: Invalid timezone %q, using UTC: %v", timezoneName, err)
		timezone = time.UTC
	}

	tenantManager, err := tenant.NewManager(logLevel, timezone, log.Default())
	if err != nil {
		log.Fatal("Failed to initialize tenant manager:", err)
	}
	tenantManager.StartAll()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/setup", tenantManager.HandleSetup)
	mux.HandleFunc("/setup/", tenantManager.HandleSetup)
	mux.HandleFunc("/mcp", tenantManager.HandleMCP)
	mux.HandleFunc("/mcp/", tenantManager.HandleMCP)
	mux.HandleFunc("/oauth/", tenantManager.HandleOAuth)
	mux.HandleFunc("/.well-known/oauth-protected-resource/", tenantManager.HandleProtectedResourceMetadata)

	httpServer := &http.Server{
		Addr:    ":" + httpPort,
		Handler: mux,
	}

	go func() {
		log.Printf("Starting server on http://0.0.0.0:%s", httpPort)
		log.Printf("- Setup URL: http://0.0.0.0:%s/setup", httpPort)
		log.Printf("- MCP endpoint format: http://0.0.0.0:%s/mcp/{TENANT_ID}", httpPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	log.Println("WhatsApp MCP running. Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("\nShutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	tenantManager.StopAll()
	log.Println("Shutdown complete")
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
