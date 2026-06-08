package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/inbound/openai"
	outbound_anthropic "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
	outbound_bedrock "github.com/mayer-tech/llmbrane/internal/outbound/bedrock"
	outbound_openai "github.com/mayer-tech/llmbrane/internal/outbound/openai"
	"github.com/mayer-tech/llmbrane/internal/router"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Initialize Gateway (Core Logic)
	gateway := core.NewGateway(cfg)

	// Register Outbound Adapters
	gateway.RegisterOutboundAdapter("openai-chat-completions", outbound_openai.NewAdapter())
	gateway.RegisterOutboundAdapter("anthropic-messages", outbound_anthropic.NewAdapter())
	gateway.RegisterOutboundAdapter("bedrock-converse", outbound_bedrock.NewAdapter())

	// Initialize Router and Middleware
	gateway.RegisterOutboundAdapter("anthropic-messages", outbound_anthropic.NewAdapter())

	// Setup Inbound Handlers
	openaiHandler := openai.NewHandler(gateway)

	// Setup Router
	mux := router.New()
	openaiHandler.RegisterRoutes(mux, "/openai")

	// Apply Middlewares
	handler := router.Chain(mux,
		router.PanicRecovery,
		router.Timeout(60*time.Second),
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
