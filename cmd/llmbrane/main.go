package main

import (
	"flag"
	"log"
	"net/http"
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
	debuglog.Init(cfg.Logging.DebugDir, debugRetention)

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

	// Register Outbound Adapters (with debug recording transport if enabled)
	transport := debuglog.NewTransport(nil)
	gateway.RegisterOutboundAdapter("openai-chat-completions", outbound_openai.NewAdapterWithTransport(requestTimeout, transport))
	gateway.RegisterOutboundAdapter("openai-responses", outbound_openairesponses.NewAdapterWithTransport(requestTimeout, transport))
	gateway.RegisterOutboundAdapter("anthropic-messages", outbound_anthropic.NewAdapterWithTransport(requestTimeout, transport))
	gateway.RegisterOutboundAdapter("bedrock-converse", outbound_bedrock.NewAdapterWithTransport(requestTimeout, transport))

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
	log.Printf("Starting llmbrane on %s", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
