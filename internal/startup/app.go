package startup

import (
	"context"
	"fmt"
	"io"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	bootstrapcommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/bootstrap"
	commandbuiltin "github.com/jonahgcarpenter/oswald-ai/internal/commands/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/compaction"
	tokenbudget "github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database/maintenance"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/extraction"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/formation"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/indexing"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/invalidation"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Error preserves the structured event and cause of an initialization failure.
type Error struct {
	Event   string
	Message string
	Cause   error
}

// Error describes the failed startup operation and its underlying cause.
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

// Unwrap exposes the original initialization error.
func (e *Error) Unwrap() error { return e.Cause }

type dependencies struct {
	databasePath string
	newRegistry  func(*config.Config, *memory.Store, *global.Store, *config.Logger) (*registry.Registry, error)
	newGateways  func(*config.Config, *accounts.Service, gatewayruntime.Dependencies, *config.Logger) ([]gateway.Service, error)
}

// Run assembles the application and waits for ctx cancellation before ordered
// cleanup. Cancellation is a normal shutdown, not an error. Gateways currently
// have no stop contract, so Run is process-oriented, not restartable in-process.
func Run(ctx context.Context, cfg *config.Config, log *config.Logger, stdout io.Writer) error {
	return run(ctx, cfg, log, stdout, dependencies{
		databasePath: config.DefaultDatabasePath,
		newRegistry:  tools.NewRegistryFromConfig,
		newGateways:  gateway.NewServicesFromConfig,
	})
}

func run(ctx context.Context, cfg *config.Config, rootLog *config.Logger, stdout io.Writer, deps dependencies) error {
	if ctx.Err() != nil {
		return nil
	}
	var cleanup shutdown
	defer cleanup.run()

	log := rootLog.Server("app")

	if cfg.LLMGatewayModel == "" {
		return &Error{Event: "app.config.invalid", Message: "missing required LLM_GATEWAY_MODEL environment variable"}
	}
	log.Info("app.model.selected", "selected LLM gateway model", config.F("model", cfg.LLMGatewayModel))

	if cfg.LLMGatewayURL == "" {
		return &Error{Event: "app.config.invalid", Message: "missing required LLM_GATEWAY_URL configuration"}
	}

	llmClient := llm.NewGatewayClient(cfg.LLMGatewayURL, cfg.LLMGatewayAPIKey, cfg.LLMGatewayVirtualKey, rootLog)

	budget := tokenbudget.NewContextBudget(cfg.ModelContextWindow, cfg.ModelMaxOutputTokens)
	log.Info("app.context_budget.configured", "configured context budget",
		config.F("model", cfg.LLMGatewayModel),
		config.F("context_window", budget.ContextWindow),
		config.F("max_output_tokens", budget.ResponseReserve),
		config.F("usable_input_limit", budget.UsableInputLimit()),
	)

	// The operator-managed soul file is read fresh for every request and used as
	// the agent's system prompt.
	soulStore := soul.NewStore(config.DefaultSoulPath)
	log.Debug("app.memory_soul.configured", "configured soul file path", config.F("path", config.DefaultSoulPath))

	// The user memory store shares the account-link database and initializes its
	// permanent schema before the other stores open their own handles.
	userMemStore, err := memory.NewSQLiteStore(deps.databasePath, llmClient, cfg.LLMGatewayEmbeddingModel, rootLog.Server("memory.user"))
	if err != nil {
		return &Error{Event: "app.memory_user.init_failed", Message: "failed to initialize user memory store", Cause: err}
	}
	cleanup.userMemory = func() { _ = userMemStore.Close() }
	retentionPolicy := config.DefaultRetentionPolicy()
	userMemStore.SetRetentionPolicy(retentionPolicy)
	log.Debug("app.memory_user.configured", "configured user memory database", config.F("path", deps.databasePath))
	globalMemStore, err := global.NewStore(deps.databasePath, llmClient, cfg.LLMGatewayEmbeddingModel, rootLog.Server("memory.global"))
	if err != nil {
		return &Error{Event: "app.memory_global.init_failed", Message: "failed to initialize global memory store", Cause: err}
	}
	cleanup.globalMemory = func() { _ = globalMemStore.Close() }
	log.Debug("app.memory_global.configured", "configured global memory database", config.F("path", deps.databasePath))
	mcpStore, err := mcp.NewStore(deps.databasePath, cfg.MCPConfigEncryptionKey, rootLog.Server("mcp.store"))
	if err != nil {
		return &Error{Event: "app.mcp.init_failed", Message: "failed to initialize MCP config store", Cause: err}
	}
	cleanup.mcpStore = func() { _ = mcpStore.Close() }
	mcpManager := mcp.NewManagerFromStore(mcpStore, rootLog)
	cleanup.mcp = func() {
		if err := mcpManager.Close(); err != nil {
			log.Warn("app.mcp.shutdown_failed", "failed to shut down MCP clients", config.ErrorField(err), config.F("status", "degraded"))
		}
	}
	accountLinkService := accounts.NewService(deps.databasePath, userMemStore, mcpManager, rootLog.Server("account_link"))
	cleanup.accounts = func() { _ = accountLinkService.Close() }
	if err := accountLinkService.Initialize(); err != nil {
		return &Error{Event: "app.account_link.init_failed", Message: "failed to initialize account link store", Cause: err}
	}
	if ctx.Err() != nil {
		return nil
	}
	bootstrapCommand, bootstrapCode, err := bootstrapcommands.New(accountLinkService)
	if err != nil {
		return &Error{Event: "app.bootstrap.init_failed", Message: "failed to initialize administrator bootstrap", Cause: err}
	}
	if bootstrapCode != "" {
		printBootstrapInstructions(stdout, bootstrapCode)
		log.Info("app.bootstrap.available", "generated first-administrator bootstrap code", config.F("status", "ok"))
	}
	indexService := indexing.NewService(userMemStore, globalMemStore, llmClient, cfg.LLMGatewayEmbeddingModel, rootLog)
	cleanup.index = indexService.Stop
	// Worker lifetimes are independent of the signal context; ordered Stop calls
	// must finish durable work before their stores close.
	indexService.Start(context.Background())
	maintenanceService := maintenance.NewService(userMemStore, retentionPolicy, rootLog)
	cleanup.maintenance = maintenanceService.Stop
	maintenanceService.Start(context.Background())
	log.Debug("app.account_link.configured", "configured account link database", config.F("path", deps.databasePath))

	toolRegistry, err := deps.newRegistry(cfg, userMemStore, globalMemStore, rootLog)
	if err != nil {
		return &Error{Event: "app.tools.init_failed", Message: "failed to initialize tools", Cause: err}
	}
	if ctx.Err() != nil {
		return nil
	}
	mcpProvider := mcp.NewProvider(mcpManager, toolRegistry.Names()...)
	formationExtractor, err := extraction.NewLLMExtractor(llmClient, cfg.LLMGatewayModel, budget.ResponseReserve)
	if err != nil {
		return &Error{Event: "app.memory_extractor.init_failed", Message: "failed to initialize background user-memory extractor", Cause: err}
	}
	formationService := formation.NewService(userMemStore, formationExtractor, cfg.LLMGatewayModel, rootLog)
	cleanup.formation = formationService.Stop
	compactor, err := compaction.NewLLMCompactor(llmClient, cfg.LLMGatewayModel, budget.ResponseReserve)
	if err != nil {
		return &Error{Event: "app.session_compactor.init_failed", Message: "failed to initialize background session compactor", Cause: err}
	}
	compactionService := compaction.NewService(userMemStore, compactor, cfg.LLMGatewayModel, budget, rootLog)
	cleanup.compaction = compactionService.Stop

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
		governance.DefaultGlobalPolicy(),
		rootLog,
		mcpProvider,
	)
	agentEngine.SetForegroundCompactor(compactor)

	// Create the broker and start its worker pool.
	// All gateways submit requests through the broker; it enforces the concurrency
	// limit and routes responses back to the originating gateway.
	requestBroker := broker.NewBroker(agentEngine, cfg.WorkerPoolSize, rootLog.Server("broker"))
	cleanup.broker = requestBroker.Shutdown
	requestBroker.Start()
	commandService, err := commandbuiltin.NewService(commandbuiltin.Dependencies{
		Accounts: accountLinkService, Memory: userMemStore, GlobalMemory: globalMemStore,
		Logger: rootLog.Server("commands"), Bootstrap: bootstrapCommand,
		MCPStore: mcpStore, MCPManager: mcpManager, Canceler: requestBroker,
	})
	if err != nil {
		return &Error{Event: "app.commands.init_failed", Message: "failed to initialize command service", Cause: err}
	}
	runtimeInvalidationBus := invalidation.NewBus()
	runtimeDeps := gatewayruntime.Dependencies{
		Broker:                 requestBroker,
		Commands:               commandService,
		Access:                 accountLinkService,
		Log:                    rootLog,
		Formation:              formationService,
		Compaction:             compactionService,
		RuntimeInvalidationBus: runtimeInvalidationBus,
	}
	activeGateways, err := deps.newGateways(cfg, accountLinkService, runtimeDeps, rootLog)
	if err != nil {
		return &Error{Event: "app.gateways.init_failed", Message: "failed to initialize gateways", Cause: err}
	}
	if ctx.Err() != nil {
		return nil
	}
	formationService.SetLowPriorityGate(requestBroker)
	compactionService.SetLowPriorityGate(requestBroker)
	formationService.Start(context.Background())
	compactionService.Start(context.Background())
	log.Info("app.start", "starting application")
	for _, gw := range activeGateways {
		go func(g gateway.Service) {
			if err := g.Start(requestBroker); err != nil {
				log.Error("app.gateway.stopped", "gateway stopped", config.F("gateway", g.Name()), config.ErrorField(err))
			}
		}(gw)
	}

	<-ctx.Done()
	log.Info("app.shutdown", "shutting down application")
	return nil
}

// Shutdown is deliberately not reverse acquisition order: maintenance stops
// before broker drain, and all workers stop before MCP clients and stores close.
type shutdown struct {
	maintenance, broker, formation, compaction, index func()
	mcp, accounts, mcpStore, globalMemory, userMemory func()
}

func (s *shutdown) run() {
	for _, stop := range []func(){s.maintenance, s.broker, s.formation, s.compaction, s.index,
		s.mcp, s.accounts, s.mcpStore, s.globalMemory, s.userMemory} {
		if stop != nil {
			stop()
		}
	}
}
