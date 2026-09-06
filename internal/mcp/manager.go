package mcp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Manager owns scoped MCP client sessions and resolves tools for active users.
type Manager struct {
	store           *Store
	sessions        map[string]*server
	userGenerations map[string]uint64
	mu              sync.Mutex
	log             *config.Logger
}

// NewManagerFromStore creates a DB-backed MCP manager.
func NewManagerFromStore(store *Store, log *config.Logger) *Manager {
	return &Manager{store: store, sessions: make(map[string]*server), userGenerations: make(map[string]uint64), log: log.Server("mcp.manager")}
}

// MergeUsersTx transfers user-scoped MCP configs in the supplied account merge transaction.
func (m *Manager) MergeUsersTx(ctx context.Context, tx *sql.Tx, winnerID, loserID string) error {
	if m == nil || m.store == nil {
		return fmt.Errorf("MCP manager is not initialized")
	}
	return m.store.MergeUsersTx(ctx, tx, winnerID, loserID)
}

// DeleteUserTx removes user-scoped MCP configs in the supplied transaction.
func (m *Manager) DeleteUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	if m == nil || m.store == nil {
		return fmt.Errorf("MCP manager is not initialized")
	}
	return m.store.DeleteUserTx(ctx, tx, userID)
}

// UserDeleteCommitted invalidates sessions owned by a deleted user.
func (m *Manager) UserDeleteCommitted(userID string) {
	if m == nil {
		return
	}
	userID = strings.TrimSpace(userID)
	prefix := ScopeUser + ":" + userID + ":"
	var closeFns []func() error
	m.mu.Lock()
	m.userGenerations[userID]++
	for key, srv := range m.sessions {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if srv != nil && srv.close != nil {
			closeFns = append(closeFns, srv.close)
		}
		delete(m.sessions, key)
	}
	m.mu.Unlock()
	for _, closeFn := range closeFns {
		m.closeSession(closeFn)
	}
}

// UserMergeCommitted invalidates sessions affected by a committed user merge.
func (m *Manager) UserMergeCommitted(winnerID, loserID string) {
	if m == nil {
		return
	}
	winnerID = strings.TrimSpace(winnerID)
	loserID = strings.TrimSpace(loserID)
	winnerPrefix := ScopeUser + ":" + winnerID + ":"
	loserPrefix := ScopeUser + ":" + loserID + ":"
	var closeFns []func() error
	m.mu.Lock()
	m.userGenerations[winnerID]++
	m.userGenerations[loserID]++
	for key, srv := range m.sessions {
		if !strings.HasPrefix(key, winnerPrefix) && !strings.HasPrefix(key, loserPrefix) {
			continue
		}
		if srv != nil && srv.close != nil {
			closeFns = append(closeFns, srv.close)
		}
		delete(m.sessions, key)
	}
	m.mu.Unlock()
	for _, closeFn := range closeFns {
		m.closeSession(closeFn)
	}
}

// ServerInfos returns global and user-scoped MCP server metadata visible to userID.
func (m *Manager) ServerInfos(ctx context.Context, userID string) []ServerInfo {
	if m == nil || m.store == nil {
		return nil
	}
	configs, err := m.store.ListForUser(ctx, userID)
	if err != nil {
		m.requestLog(ctx).Warn("mcp.server_configs.list_failed", "failed to list MCP servers", config.F("status", "degraded"), config.ErrorField(err))
		return nil
	}
	infos := make([]ServerInfo, 0, len(configs))
	for _, cfg := range configs {
		info := ServerInfo{Name: cfg.Name, Description: cfg.Description, Scope: cfg.Scope, OwnerUserID: cfg.OwnerUserID, Status: serverStatusNotConnected}
		if !cfg.Enabled {
			info.Status = serverStatusDisabled
			infos = append(infos, info)
			continue
		}
		if srv := m.cached(scopeKey(cfg)); srv != nil {
			if srv.reason != "" {
				info.Status = serverStatusError
				info.Reason = srv.reason
			} else {
				info.Status = serverStatusConnected
				info.ToolCount = len(srv.tools)
			}
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].Scope != infos[j].Scope {
			return infos[i].Scope < infos[j].Scope
		}
		return infos[i].Name < infos[j].Name
	})
	return infos
}

// ServerInfo returns a visible server by name for the active user.
func (m *Manager) ServerInfo(ctx context.Context, userID string, name string) (ServerInfo, bool) {
	name = strings.TrimSpace(strings.ToLower(name))
	for _, info := range m.ServerInfos(ctx, userID) {
		if info.Name == name {
			return info, true
		}
	}
	return ServerInfo{}, false
}

// ToolSpecs returns currently connected tools visible to userID.
func (m *Manager) ToolSpecs(ctx context.Context, userID string) []ToolSpec {
	configs, err := m.store.ListForUser(ctx, userID)
	if err != nil {
		m.requestLog(ctx).Warn("mcp.server_configs.list_failed", "failed to list MCP servers", config.F("status", "degraded"), config.ErrorField(err))
		return nil
	}
	var specs []ToolSpec
	for _, cfg := range configs {
		if !cfg.Enabled || strings.TrimSpace(cfg.Description) == "" || isReservedServerName(cfg.Name) {
			continue
		}
		srv, err := m.ensureConnected(ctx, cfg)
		if err != nil {
			continue
		}
		specs = append(specs, srv.tools...)
	}
	return specs
}

// ServerToolSpecs returns tools for a single visible server, connecting lazily.
func (m *Manager) ServerToolSpecs(ctx context.Context, userID, name string) ([]ToolSpec, ServerInfo, error) {
	if isReservedServerName(name) {
		return nil, ServerInfo{}, fmt.Errorf("MCP server name %q is reserved", name)
	}
	cfg, ok, err := m.resolveConfig(ctx, userID, name)
	if err != nil {
		m.requestLog(ctx).Warn("mcp.server_config.resolve_failed", "failed to resolve MCP server", config.F("status", "degraded"), config.ErrorField(err))
		return nil, ServerInfo{}, err
	}
	if !ok {
		m.requestLog(ctx).Debug("mcp.server_config.not_found", "MCP server is not configured", config.F("status", "rejected"))
		return nil, ServerInfo{}, fmt.Errorf("no configured MCP server named %q", name)
	}
	info := ServerInfo{Name: cfg.Name, Description: cfg.Description, Scope: cfg.Scope, OwnerUserID: cfg.OwnerUserID, Status: serverStatusNotConnected}
	if !cfg.Enabled {
		info.Status = serverStatusDisabled
		return nil, info, nil
	}
	srv, err := m.ensureConnected(ctx, cfg)
	if err != nil {
		info.Status = serverStatusError
		info.Reason = config.SafeErrorText(err)
		return nil, info, nil
	}
	info.Status = serverStatusConnected
	info.ToolCount = len(srv.tools)
	return append([]ToolSpec(nil), srv.tools...), info, nil
}

// Execute calls a scoped MCP tool visible to userID.
func (m *Manager) Execute(ctx context.Context, userID string, toolName string, args map[string]interface{}) (ExecutionResult, error) {
	serverName, remoteName, ok := splitToolName(toolName)
	if !ok {
		return ExecutionResult{}, fmt.Errorf("invalid MCP tool name %q", toolName)
	}
	tols, _, err := m.ServerToolSpecs(ctx, userID, serverName)
	if err != nil {
		return ExecutionResult{}, err
	}
	for _, tool := range tols {
		if tool.RemoteName == remoteName || tool.Name == toolName {
			result, err := tool.Handler(ctx, args)
			return ExecutionResult{Result: result, ServerID: tool.ServerID, ServerName: tool.Server, Scope: tool.Scope, OwnerUserID: tool.OwnerUserID, ToolName: tool.Name, RemoteToolName: tool.RemoteName}, err
		}
	}
	return ExecutionResult{}, fmt.Errorf("MCP tool %q is not available", toolName)
}

// Close shuts down connected MCP sessions.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for key, srv := range m.sessions {
		if srv.close != nil {
			if err := m.closeSession(srv.close); err != nil {
				errs = append(errs, fmt.Errorf("close %s MCP session: %w", key, err))
			}
		}
		delete(m.sessions, key)
	}
	return errors.Join(errs...)
}

// Invalidate closes any cached session for a server whose config changed.
func (m *Manager) Invalidate(scope, ownerUserID, name string) {
	if m == nil {
		return
	}
	key := scope + ":" + strings.TrimSpace(name)
	if scope == ScopeUser {
		key = scope + ":" + strings.TrimSpace(ownerUserID) + ":" + strings.TrimSpace(name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if srv := m.sessions[key]; srv != nil && srv.close != nil {
		m.closeSession(srv.close)
	}
	delete(m.sessions, key)
}

func (m *Manager) cached(key string) *server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[key]
}

func (m *Manager) resolveConfig(ctx context.Context, userID, name string) (ServerConfig, bool, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if cfg, ok, err := m.store.Get(ctx, ScopeGlobal, "", name); err != nil || ok {
		return cfg, ok, err
	}
	if strings.TrimSpace(userID) == "" {
		return ServerConfig{}, false, nil
	}
	return m.store.Get(ctx, ScopeUser, userID, name)
}

func (m *Manager) ensureConnected(ctx context.Context, cfg ServerConfig) (_ *server, err error) {
	key := scopeKey(cfg)
	m.mu.Lock()
	srv := m.sessions[key]
	generation := m.userGenerations[cfg.OwnerUserID]
	m.mu.Unlock()
	if srv != nil && srv.reason == "" {
		return srv, nil
	}
	started := time.Now()
	meta := requestctx.MetadataFromContext(ctx)
	parent := meta.OperationID
	if parent == "" {
		parent = meta.ParentOperationID
	}
	meta.OperationID, meta.ParentOperationID = rand.Text(), parent
	ctx = requestctx.WithMetadata(ctx, meta)
	connectionLog := m.requestLog(ctx).With(config.F("server_id", cfg.ID), config.F("scope", cfg.Scope))
	phase := "validate"
	defer func() {
		status, outcome := "ok", "ok"
		if err != nil {
			status, outcome = "error", "error"
			if errors.Is(err, context.Canceled) {
				status, outcome = "ok", "canceled"
			}
		}
		fields := []config.Field{config.F("record_kind", "measurement"), config.F("operation", "connect"), config.F("phase", phase), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", status), config.F("outcome", outcome)}
		if err != nil {
			fields = append(fields, config.ErrorField(err))
		} else if srv != nil {
			fields = append(fields, config.F("tool_count", len(srv.tools)))
		}
		connectionLog.Info("mcp.server.connect.complete", "MCP connection completed", fields...)
		if err != nil && !errors.Is(err, context.Canceled) {
			connectionLog.Warn("mcp.server.connect_failed", "MCP server unavailable", config.F("status", "degraded"), config.ErrorField(err))
		}
	}()
	if cfg.Scope == ScopeUser {
		current, ok, err := m.store.Get(ctx, cfg.Scope, cfg.OwnerUserID, cfg.Name)
		if err != nil {
			return nil, err
		}
		if !ok || current.ID != cfg.ID {
			return nil, fmt.Errorf("MCP server ownership changed before connecting")
		}
		cfg = current
	}
	if _, err := parseAndValidateURL(ctx, cfg.URL, m.store.resolver); err != nil {
		m.rememberError(key, cfg, generation, err)
		return nil, err
	}
	if cfg.Transport != TransportStreamableHTTP {
		err := fmt.Errorf("MCP transport %q is not implemented", cfg.Transport)
		m.rememberError(key, cfg, generation, err)
		return nil, err
	}
	phase = "connect"
	session, closeFn, err := connectStreamableHTTP(ctx, cfg)
	if err != nil {
		m.rememberError(key, cfg, generation, err)
		return nil, err
	}
	phase = "catalog"
	tools, err := loadToolSpecs(ctx, cfg, session, m.log)
	if err != nil {
		m.closeSession(closeFn)
		m.rememberError(key, cfg, generation, err)
		return nil, err
	}
	srv = &server{config: cfg, tools: tools, close: closeFn}
	m.mu.Lock()
	if cfg.Scope == ScopeUser && m.userGenerations[cfg.OwnerUserID] != generation {
		m.mu.Unlock()
		m.closeSession(closeFn)
		return nil, fmt.Errorf("MCP server ownership changed while connecting")
	}
	if old := m.sessions[key]; old != nil && old.close != nil {
		m.closeSession(old.close)
	}
	m.sessions[key] = srv
	m.mu.Unlock()
	return srv, nil
}

func (m *Manager) requestLog(ctx context.Context) *config.Logger {
	return m.log.With(requestctx.LogFields(ctx)...)
}

func (m *Manager) closeSession(closeFn func() error) error {
	started := time.Now()
	err := closeFn()
	fields := []config.Field{config.F("operation_id", rand.Text()), config.F("operation", "close"), config.F("duration_ms", time.Since(started).Milliseconds())}
	if err != nil {
		m.log.Warn("mcp.server.close_failed", "failed to close MCP session", append(fields, config.F("status", "degraded"), config.ErrorField(err))...)
	} else {
		m.log.Info("mcp.server.close.complete", "closed MCP session", append(fields, config.F("status", "ok"))...)
	}
	return err
}

func (m *Manager) rememberError(key string, cfg ServerConfig, generation uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg.Scope == ScopeUser && m.userGenerations[cfg.OwnerUserID] != generation {
		return
	}
	m.sessions[key] = &server{config: cfg, reason: config.SafeErrorText(err)}
}

func connectStreamableHTTP(ctx context.Context, cfg ServerConfig) (*gomcp.ClientSession, func() error, error) {
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &headerTransport{
			base:    http.DefaultTransport,
			headers: cfg.Headers,
		},
	}
	client := gomcp.NewClient(&gomcp.Implementation{Name: "oswald-ai", Version: "1.0.0"}, &gomcp.ClientOptions{Capabilities: &gomcp.ClientCapabilities{}})
	transport := &gomcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient, DisableStandaloneSSE: true}
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect MCP session: %w", err)
	}
	return session, session.Close, nil
}

func loadToolSpecs(ctx context.Context, cfg ServerConfig, session *gomcp.ClientSession, log *config.Logger) ([]ToolSpec, error) {
	var specs []ToolSpec
	skipped := 0
	defer func() {
		if skipped > 0 && ctx.Err() == nil {
			log.With(requestctx.LogFields(ctx)...).Warn("mcp.tool.skipped", "skipped unsupported MCP catalog entries", config.F("skipped_count", skipped), config.F("status", "degraded"))
		}
	}()
	cursor := ""
	for {
		result, err := session.ListTools(ctx, &gomcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("list MCP tools: %w", err)
		}
		for _, tool := range result.Tools {
			if tool == nil {
				skipped++
				continue
			}
			if strings.EqualFold(strings.TrimSpace(tool.Name), "tools") {
				skipped++
				continue
			}
			spec, err := toolSpec(cfg, tool, session, log)
			if err != nil {
				skipped++
				continue
			}
			specs = append(specs, spec)
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return specs, nil
}

func toolSpec(cfg ServerConfig, tool *gomcp.Tool, session *gomcp.ClientSession, log *config.Logger) (ToolSpec, error) {
	remoteName := strings.TrimSpace(tool.Name)
	if err := validateProviderIdentifier(remoteName); err != nil {
		return ToolSpec{}, fmt.Errorf("invalid remote tool name: %w", err)
	}
	localName := cfg.Name + "." + remoteName
	if len(localName) > maxProviderIdentifierBytes {
		return ToolSpec{}, fmt.Errorf("qualified tool name exceeds %d bytes", maxProviderIdentifierBytes)
	}
	params, err := schemaToParams(tool.InputSchema)
	if err != nil {
		return ToolSpec{}, fmt.Errorf("normalize input schema: %w", err)
	}
	description := normalizeCatalogDescription(tool.Description, maxToolDescriptionRuneCount)
	if description == "" {
		description = normalizeCatalogDescription(tool.Title, maxToolDescriptionRuneCount)
	}
	return ToolSpec{Name: localName, Description: description, ServerID: cfg.ID, Server: cfg.Name, Scope: cfg.Scope, OwnerUserID: cfg.OwnerUserID, RemoteName: remoteName, Parameters: params, Handler: func(ctx context.Context, arguments map[string]interface{}) (governance.Result, error) {
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		reqLog := log.Agent("agent.tool.mcp", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		reqLog.Debug("agent.tool.mcp.start", "starting MCP tool execution", config.F("tool_name", localName), config.F("remote_tool_name", remoteName), config.F("server", cfg.Name), config.F("scope", cfg.Scope))
		result, err := session.CallTool(ctx, &gomcp.CallToolParams{Name: remoteName, Arguments: arguments})
		if err != nil {
			return governance.Result{}, safeMCPRequestError(err)
		}
		flattened, err := flattenToolResult(result)
		if err != nil {
			return governance.Result{}, err
		}
		return flattened, nil
	}}, nil
}

func safeMCPRequestError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("MCP request was canceled (category: canceled). Do not retry automatically")
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("MCP request timed out (category: timeout). Retry once")
	default:
		return fmt.Errorf("MCP request failed (category: transport). Retry once; if it fails again, check server availability")
	}
}

func scopeKey(cfg ServerConfig) string {
	if cfg.Scope == ScopeGlobal {
		return ScopeGlobal + ":" + cfg.Name
	}
	return ScopeUser + ":" + cfg.OwnerUserID + ":" + cfg.Name
}

func splitToolName(name string) (string, string, bool) {
	server, remote, ok := strings.Cut(strings.TrimSpace(name), ".")
	return server, remote, ok && server != "" && remote != ""
}
