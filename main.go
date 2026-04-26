package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/openclaw/whatsapp/api"
	"github.com/openclaw/whatsapp/bridge"
	"github.com/openclaw/whatsapp/config"
	"github.com/openclaw/whatsapp/store"
)

var version = "v0.2.0"

func main() {
	root := &cobra.Command{
		Use:   "openclaw-whatsapp",
		Short: "WhatsApp bridge for OpenClaw agents",
	}

	// --- start command -------------------------------------------------------
	var configPath string
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the WhatsApp bridge service",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(configPath)
		},
	}
	startCmd.Flags().StringVarP(&configPath, "config", "c", "config.yaml", "Path to config file")
	root.AddCommand(startCmd)

	// --- status command ------------------------------------------------------
	var statusAddr string
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Check the bridge connection status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(statusAddr)
		},
	}
	statusCmd.Flags().StringVar(&statusAddr, "addr", "http://localhost:8555", "Bridge HTTP address")
	root.AddCommand(statusCmd)

	// --- send command --------------------------------------------------------
	var sendAddr string
	sendCmd := &cobra.Command{
		Use:   "send [number] [message]",
		Short: "Send a text message",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSend(sendAddr, args[0], args[1])
		},
	}
	sendCmd.Flags().StringVar(&sendAddr, "addr", "http://localhost:8555", "Bridge HTTP address")
	root.AddCommand(sendCmd)

	// --- stop command --------------------------------------------------------
	var stopAddr string
	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the bridge (sends shutdown signal)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop(stopAddr)
		},
	}
	stopCmd.Flags().StringVar(&stopAddr, "addr", "http://localhost:8555", "Bridge HTTP address")
	root.AddCommand(stopCmd)

	// --- version command -----------------------------------------------------
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("openclaw-whatsapp %s\n", version)
		},
	})

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// runStart is the main service entrypoint that wires all components together.
func runStart(configPath string) error {
	// 1. Load config
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := cfg.EnsureDataDir(); err != nil {
		return fmt.Errorf("ensure data dir: %w", err)
	}

	// 2. Setup logger
	var logLevel slog.Level
	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(log)

	log.Info("starting openclaw-whatsapp", "version", version, "port", cfg.Port, "data_dir", cfg.DataDir)

	// 3. Open message store
	dbPath := filepath.Join(cfg.DataDir, "messages.db")
	msgStore, err := store.NewMessageStore(dbPath)
	if err != nil {
		return fmt.Errorf("open message store: %w", err)
	}
	defer msgStore.Close()

	// 4. Create bridge client
	client, err := bridge.NewClient(cfg.DataDir, log)
	if err != nil {
		return fmt.Errorf("create bridge client: %w", err)
	}

	// 5. Create webhook sender
	webhookFilters := bridge.WebhookFilters{
		DMOnly:       cfg.WebhookFilters.DMOnly,
		IgnoreGroups: cfg.WebhookFilters.IgnoreGroups,
	}
	webhookURL := cfg.WebhookURL
	if webhookURL == "" {
		webhookURL = "http://127.0.0.1:8000/webhook/whatsapp"
		log.Info("using default webhook url", "url", webhookURL)
	}
	webhook := bridge.NewWebhookSender(webhookURL, webhookFilters, log)

	// 5b. Create agent trigger
	agent := bridge.NewAgentTrigger(
		cfg.Agent.Enabled,
		cfg.Agent.Mode,
		cfg.Agent.Command,
		cfg.Agent.HTTPURL,
		cfg.Agent.ReplyEndpoint,
		cfg.Agent.SystemPrompt,
		cfg.Agent.IgnoreFromMe,
		cfg.Agent.DMOnly,
		cfg.Agent.Allowlist,
		cfg.Agent.Blocklist,
		cfg.Agent.Timeout.Duration,
		log,
	)
	if cfg.Agent.Enabled {
		log.Info("agent mode enabled", "mode", cfg.Agent.Mode)
	}

	// 6. Wire event handler
	handler := bridge.MakeEventHandler(client, msgStore, webhook, agent, log)
	client.SetEventHandler(handler)

	// 7. Connect to WhatsApp
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connect to WhatsApp: %w", err)
	}

	// 8. Start reconnect loop
	if cfg.AutoReconnect {
		bridge.StartReconnectLoop(ctx, client, cfg.ReconnectInterval.Duration, log)
	}

	// 9. Start HTTP server
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: api.NewRouter(&api.Server{
			Client:  client,
			Store:   msgStore,
			Log:     log,
			Version: version,
		}),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Info("HTTP server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	log.Info("bridge is running", "qr_url", fmt.Sprintf("http://localhost:%d/qr", cfg.Port))

	// 10. Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down...")
	cancel()
	client.Disconnect()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server shutdown error", "error", err)
	}

	log.Info("goodbye")
	return nil
}

// runStatus queries the bridge HTTP status endpoint.
func runStatus(addr string) error {
	resp, err := http.Get(addr + "/status")
	if err != nil {
		return fmt.Errorf("failed to reach bridge at %s: %w", addr, err)
	}
	defer resp.Body.Close()

	var buf [4096]byte
	n, _ := resp.Body.Read(buf[:])
	fmt.Println(string(buf[:n]))
	return nil
}

// runSend sends a text message via the bridge HTTP API.
func runSend(addr, to, message string) error {
	body := fmt.Sprintf(`{"to":%q,"message":%q}`, to, message)
	resp, err := http.Post(addr+"/send/text", "application/json", strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	defer resp.Body.Close()

	var buf [4096]byte
	n, _ := resp.Body.Read(buf[:])
	fmt.Println(string(buf[:n]))
	return nil
}

// runStop is a placeholder — in practice you'd signal via PID file or an admin endpoint.
func runStop(addr string) error {
	fmt.Println("To stop the bridge, send SIGTERM to the running process.")
	fmt.Println("For example: kill $(pgrep openclaw-whatsapp)")
	return nil
}
