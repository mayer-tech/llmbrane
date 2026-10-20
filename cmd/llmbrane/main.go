package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/debuglog"
	inbound_anthropic "github.com/mayer-tech/llmbrane/internal/inbound/anthropic"
	inbound_openai "github.com/mayer-tech/llmbrane/internal/inbound/openai"
	"github.com/mayer-tech/llmbrane/internal/logger"
	outbound_anthropic "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
	outbound_bedrock "github.com/mayer-tech/llmbrane/internal/outbound/bedrock"
	outbound_openai "github.com/mayer-tech/llmbrane/internal/outbound/openai"
	outbound_openairesponses "github.com/mayer-tech/llmbrane/internal/outbound/openairesponses"
	httpretry "github.com/mayer-tech/llmbrane/internal/outbound/retry"
	"github.com/mayer-tech/llmbrane/internal/router"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Initialize Logger
	logger.Init(cfg.Logging.Level)

	// Initialize Debug File Logger
	var debugRetention time.Duration
	if cfg.Logging.DebugRetention != "" {
		if d, err := time.ParseDuration(cfg.Logging.DebugRetention); err == nil {
			debugRetention = d
		} else {
			log.Printf("warning: invalid debug_retention %q in config: %v", cfg.Logging.DebugRetention, err)
		}
	}
	if debugRetention <= 0 {
		debugRetention = 24 * time.Hour
	}
	if err := debuglog.Init(cfg.Logging.DebugDir, debugRetention); err != nil {
		log.Printf("warning: debug file logging disabled: %v", err)
	} else if debuglog.Enabled() {
		log.Printf("debug logging enabled: dir=%s retention=%s", debuglog.Dir(), debugRetention)
		debuglog.LogRuntime("llmbrane starting")
	}

	// Initialize Gateway (Core Logic)
	gateway := core.NewGateway(cfg)

	// Parse Request / Idle Timeout
	var requestTimeout time.Duration
	if cfg.Server.RequestTimeout != "" {
		if d, err := time.ParseDuration(cfg.Server.RequestTimeout); err == nil {
			requestTimeout = d
		} else {
			log.Printf("warning: invalid request_timeout %q in config: %v", cfg.Server.RequestTimeout, err)
		}
	}
	if requestTimeout <= 0 {
		requestTimeout = 60 * time.Second
	}

	// Build the provider-side HTTP 429 retry policy from configuration
	// (defaults: 6 retries, 1s base delay, 250ms jitter, 60s max delay)
	retryPolicy := httpretry.DefaultPolicy
	if r := cfg.Retry; r != nil {
		if r.MaxRetries != nil {
			retryPolicy.MaxRetries = *r.MaxRetries
		}
		if d, ok := parseOptionalDuration("retry.base_delay", r.BaseDelay); ok {
			retryPolicy.BaseDelay = d
		}
		if d, ok := parseOptionalDuration("retry.jitter", r.Jitter); ok {
			retryPolicy.Jitter = d
		}
		if d, ok := parseOptionalDuration("retry.max_delay", r.MaxDelay); ok {
			retryPolicy.MaxDelay = d
		}
	}

	// Register Outbound Adapters (with debug recording transport if enabled)
	transport := debuglog.NewTransport(nil)
	gateway.RegisterOutboundAdapter("openai-chat-completions", outbound_openai.NewAdapterWithPolicy(requestTimeout, transport, retryPolicy))
	gateway.RegisterOutboundAdapter("openai-responses", outbound_openairesponses.NewAdapterWithPolicy(requestTimeout, transport, retryPolicy))
	gateway.RegisterOutboundAdapter("anthropic-messages", outbound_anthropic.NewAdapterWithPolicy(requestTimeout, transport, retryPolicy))
	gateway.RegisterOutboundAdapter("bedrock-converse", outbound_bedrock.NewAdapterWithPolicy(requestTimeout, transport, retryPolicy))

	// Setup Inbound Handlers
	openaiHandler := inbound_openai.NewHandler(gateway)
	anthropicHandler := inbound_anthropic.NewHandler(gateway)

	// Setup Router
	mux := router.New()
	openaiHandler.RegisterRoutes(mux, "/openai")
	anthropicHandler.RegisterRoutes(mux, "/anthropic")

	// Apply Middlewares
	handler := router.Chain(mux,
		router.PanicRecovery,
		router.RequestPath,
		router.DebugLog,
	)

	// Start Server
	addr := cfg.Server.Listen
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{Addr: addr, Handler: handler}

	// Graceful shutdown on SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %s, shutting down", sig)
		debuglog.LogRuntime(fmt.Sprintf("llmbrane stopping (signal: %s)", sig))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Printf("starting llmbrane on %s", addr)
	debuglog.LogRuntime(fmt.Sprintf("llmbrane listening on %s", addr))
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		debuglog.LogRuntime(fmt.Sprintf("llmbrane server error: %v", err))
		log.Fatalf("server error: %v", err)
	}
	debuglog.LogRuntime("llmbrane stopped")
}

// parseOptionalDuration parses an optional config duration and warns on
// invalid values. Empty values fall back to the built-in default.
func parseOptionalDuration(name, value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		log.Printf("warning: invalid %s %q in config: %v", name, value, err)
		return 0, false
	}
	return d, true
}
