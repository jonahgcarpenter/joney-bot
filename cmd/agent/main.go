package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	bootstrapcommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/bootstrap"
	commandbuiltin "github.com/jonahgcarpenter/oswald-ai/internal/commands/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/formationruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/indexruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/maintenanceruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryextractor"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/runtimeinvalidation"
	"github.com/jonahgcarpenter/oswald-ai/internal/sessionruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/startup"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/globalmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func main() {
	// Load config
	cfg, err := config.Load()
	startup.PrintBanner(os.Stdout)
	if err != nil {
		config.NewLogger(config.LevelInfo).Server("app").Fatal("app.config.invalid", "invalid runtime configuration", config.ErrorField(err))
	}

	// Initialize logger — all components receive this instance
	rootLog := config.NewLogger(cfg.LogLevel)
	log := rootLog.Server("app")
	log.Debug("app.config.loaded", "loaded runtime configuration", config.F("log_level", cfg.LogLevel.String()))

	if cfg.LLMGatewayModel == "" {
		log.Fatal("app.config.invalid", "missing required LLM_GATEWAY_MODEL environment variable")
	}
	log.Info("app.model.selected", "selected LLM gateway model", config.F("model", cfg.LLMGatewayModel))

	if cfg.LLMGatewayURL == "" {
		log.Fatal("app.config.invalid", "missing required LLM_GATEWAY_URL configuration")
	}

	llmClient := llm.NewGatewayClient(cfg.LLMGatewayURL, cfg.LLMGatewayAPIKey, cfg.LLMGatewayVirtualKey, rootLog)

	budget := promptbudget.NewContextBudget(cfg.ModelContextWindow, cfg.ModelMaxOutputTokens)
	log.Info("app.context_budget.configured", "configured context budget",
		config.F("model", cfg.LLMGatewayModel),
		config.F("context_window", budget.ContextWindow),
		config.F("max_output_tokens", budget.ResponseReserve),
		config.F("usable_input_limit", budget.UsableInputLimit()),
		config.F("prompt_budget", budget.PromptBudget()),
	)

	// The operator-managed soul file is read fresh for every request and used as
	// the agent's system prompt.
	soulStore := soul.NewStore(config.DefaultSoulPath)
	log.Debug("app.memory_soul.configured", "configured soul file path", config.F("path", config.DefaultSoulPath))

	// The user memory store shares the account-link database and initializes its
	// permanent schema before the other stores open their own handles.
	userMemStore, err := usermemory.NewSQLiteStore(config.DefaultAccountLinkPath, llmClient, cfg.LLMGatewayEmbeddingModel, rootLog.Server("memory.user"))
	if err != nil {
		log.Fatal("app.memory_user.init_failed", "failed to initialize user memory store", config.ErrorField(err))
	}
	defer userMemStore.Close() // nolint:errcheck
	userMemStore.SetRetentionPolicy(cfg.RetentionPolicy)
	log.Debug("app.memory_user.configured", "configured user memory database", config.F("path", config.DefaultAccountLinkPath))
	globalMemStore, err := globalmemory.NewStore(config.DefaultAccountLinkPath, llmClient, cfg.LLMGatewayEmbeddingModel, rootLog.Server("memory.global"))
	if err != nil {
		log.Fatal("app.memory_global.init_failed", "failed to initialize global memory store", config.ErrorField(err))
	}
	defer globalMemStore.Close() // nolint:errcheck
	log.Debug("app.memory_global.configured", "configured global memory database", config.F("path", config.DefaultAccountLinkPath))
	mcpStore, err := mcp.NewStore(config.DefaultAccountLinkPath, cfg.MCPConfigEncryptionKey, rootLog.Server("mcp.store"))
	if err != nil {
		log.Fatal("app.mcp.init_failed", "failed to initialize MCP config store", config.ErrorField(err))
	}
	defer mcpStore.Close() // nolint:errcheck
	mcpManager := mcp.NewManagerFromStore(mcpStore, rootLog)
	accountLinkService := accountlinking.NewService(config.DefaultAccountLinkPath, userMemStore, mcpManager, rootLog.Server("account_link"))
	if err := accountLinkService.Initialize(); err != nil {
		log.Fatal("app.account_link.init_failed", "failed to initialize account link store", config.ErrorField(err))
	}
	defer accountLinkService.Close() // nolint:errcheck
	userMemStore.SetSpeakerLineResolver(accountLinkService.SpeakerLine)
	bootstrapCommand, bootstrapCode, err := bootstrapcommands.New(accountLinkService)
	if err != nil {
		log.Fatal("app.bootstrap.init_failed", "failed to initialize administrator bootstrap", config.ErrorField(err))
	}
	if bootstrapCode != "" {
		fmt.Fprintf(os.Stdout, "\nOswald first-administrator bootstrap\n\nBootstrap code: %s\n\nRun /bootstrap %s from an authenticated Discord, iMessage, or Home Assistant account. The code is valid once for this process. Restart Oswald to replace a lost code while no administrator exists.\n\n", bootstrapCode, bootstrapCode)
		log.Info("app.bootstrap.available", "generated first-administrator bootstrap code", config.F("status", "ok"))
	}
	indexService := indexruntime.NewService(userMemStore, globalMemStore, llmClient, cfg.LLMGatewayEmbeddingModel, rootLog)
	indexService.Start(context.Background())
	maintenanceService := maintenanceruntime.NewService(userMemStore, cfg.RetentionPolicy, rootLog)
	maintenanceService.Start(context.Background())
	log.Debug("app.account_link.configured", "configured account link database", config.F("path", config.DefaultAccountLinkPath))

	toolRegistry, err := tools.NewRegistryFromConfig(cfg, userMemStore, globalMemStore, rootLog)
	if err != nil {
		log.Fatal("app.tools.init_failed", "failed to initialize tools", config.ErrorField(err))
	}
	mcpProvider := mcp.NewProvider(mcpManager, toolRegistry.Names()...)
	formationExtractor, err := memoryextractor.NewLLMExtractor(llmClient, cfg.LLMGatewayModel, budget.ResponseReserve)
	if err != nil {
		log.Fatal("app.memory_extractor.init_failed", "failed to initialize background user-memory extractor", config.ErrorField(err))
	}
	formationService := formationruntime.NewService(userMemStore, formationExtractor, cfg.LLMGatewayModel, rootLog)
	compactionExtractor, err := sessionruntime.NewLLMExtractor(llmClient, cfg.LLMGatewayModel, budget.ResponseReserve)
	if err != nil {
		log.Fatal("app.session_compactor.init_failed", "failed to initialize background session compactor", config.ErrorField(err))
	}
	compactionService := sessionruntime.NewService(userMemStore, compactionExtractor, cfg.LLMGatewayModel, budget, rootLog)

	if cfg.LLMGatewayEmbeddingModel != "" {
		log.Info("app.memory_vector.enabled", "enabled semantic durable-memory retrieval",
			config.F("embedding_model", cfg.LLMGatewayEmbeddingModel),
		)
	} else {
		log.Debug("app.memory_vector.disabled", "semantic durable-memory retrieval disabled")
	}

	agentEngine := agent.NewAgent(
		llmClient,
		toolRegistry,
		cfg.LLMGatewayModel,
		soulStore,
		userMemStore,
		budget,
		governance.GlobalPolicy{
			MaxExecutions:          cfg.MaxToolCallsPerRequest,
			MaxToolIterations:      cfg.MaxToolIterations,
			MaxConsecutiveFailures: cfg.MaxToolFailureRetries,
		},
		rootLog,
		mcpProvider,
	)
	agentEngine.SetForegroundCompactor(compactionExtractor)

	// Create the broker and start its worker pool.
	// All gateways submit requests through the broker; it enforces the concurrency
	// limit and routes responses back to the originating gateway.
	requestBroker := broker.NewBroker(agentEngine, cfg.WorkerPoolSize, rootLog.Server("broker"))
	requestBroker.Start()
	commandService, err := commandbuiltin.NewServiceWithGlobalMemory(accountLinkService, userMemStore, globalMemStore, rootLog.Server("commands"), bootstrapCommand, commandbuiltin.MCPDeps{Store: mcpStore, Manager: mcpManager, Canceler: requestBroker})
	if err != nil {
		log.Fatal("app.commands.init_failed", "failed to initialize command service", config.ErrorField(err))
	}
	runtimeInvalidationBus := runtimeinvalidation.NewBus()
	runtimeDeps := gatewayruntime.Dependencies{
		Broker:                 requestBroker,
		Commands:               commandService,
		Access:                 accountLinkService,
		Log:                    rootLog,
		Formation:              formationService,
		Compaction:             compactionService,
		RuntimeInvalidationBus: runtimeInvalidationBus,
	}
	activeGateways, err := gateway.NewServicesFromConfig(cfg, accountLinkService, runtimeDeps, rootLog)
	if err != nil {
		log.Fatal("app.gateways.init_failed", "failed to initialize gateways", config.ErrorField(err))
	}
	formationService.SetLowPriorityGate(requestBroker)
	compactionService.SetLowPriorityGate(requestBroker)
	formationService.Start(context.Background())
	compactionService.Start(context.Background())
	// Boot up all registered gateways dynamically
	log.Info("app.start", "starting application")
	for _, gw := range activeGateways {
		// Pass 'gw' into the closure to avoid loop variable capture bugs
		go func(g gateway.Service) {
			if err := g.Start(requestBroker); err != nil {
				log.Error("app.gateway.stopped", "gateway stopped", config.F("gateway", g.Name()), config.ErrorField(err))
			}
		}(gw)
	}

	// This keeps main() alive while the gateways run in the background goroutines
	stop := make(chan os.Signal, 1)

	// Listen for standard termination signals (Ctrl+C, Docker stop, Kubernetes SIGTERM)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	<-stop // The main thread will pause here indefinitely until a signal is received

	log.Info("app.shutdown", "shutting down application")
	maintenanceService.Stop()
	// Drain the broker: stop accepting new requests and wait for all in-flight
	// Process() calls to complete before the process exits.
	requestBroker.Shutdown()
	formationService.Stop()
	compactionService.Stop()
	indexService.Stop()
	if err := mcpManager.Close(); err != nil {
		log.Warn("app.mcp.shutdown_failed", "failed to shut down MCP clients", config.ErrorField(err), config.F("status", "degraded"))
	}
}
