# AGENTS.md - Oswald AI Developer Reference

This is the implementation and contributor reference for the current codebase. `README.md` describes the product and user commands. `.env.example` is the canonical application environment-variable inventory; sample values are not necessarily runtime defaults. Source code, permanent migrations, and tests define behavior when documentation disagrees.

Keep this document current when changing architecture, authorization, persistence, provider contracts, or operational limits. Describe implemented behavior, not planned features. Preserve documentation of compatibility paths that still read persisted data; do not maintain a history of removed features here.

## Project And Verification

Oswald is a Go application with one iterative LLM-backed agent, exposed through Discord, iMessage/BlueBubbles, and an optional Home Assistant WebSocket gateway. Discord and iMessage accept current-turn images; Home Assistant accepts text only. There is no JavaScript, TypeScript, or frontend application in this repository.

The module declares Go 1.25.7. SQLite, sqlite-vec, and image decoding include native/CGO dependencies. Use a working C/C++ toolchain, CGO, and the `sqlite_fts5` build tag. Discord GIFV extraction additionally uses `ffmpeg` and `ffprobe`.

```bash
go run -tags sqlite_fts5 ./cmd/agent/main.go
go build -tags sqlite_fts5 ./...
go test -tags sqlite_fts5 ./...
go test -race -tags sqlite_fts5 ./...
go vet -tags sqlite_fts5 ./...
gofmt -w .
git diff --check
```

Run focused tests while editing and the full suite for cross-package changes. Use `-count=1` when an uncached run is needed. A different installed Go toolchain can be overridden per command with `GOTOOLCHAIN=go1.25.7`. Do not run the real application to test a refactor against local credentials or the production database.

The checked-in release workflow, `.github/workflows/docker-image.yml`, runs tagged tests before building and publishing the container on published releases. It does not currently enforce race, vet, or formatting checks on every PR; run those locally when appropriate.

## Directory Ownership

Use shallow domain grouping. Separate files by responsibility within a package before creating new packages. Subpackages are justified by distinct ownership or dependency direction, not by an arbitrary line-count limit.

```text
cmd/agent/                    Process entry, signals, final exit handling
data/
  memory/soul/                Operator-managed system prompt
  tools/                      Markdown builtin tool schemas
  workflows/comfyui/          Supported operator workflow templates
  database/                  Runtime database, not test fixtures
internal/
  accounts/                  Identity resolution, linking, moderation, merge coordination
  agent/                     Foreground loop, prompt assembly, streaming, tool execution
  broker/                    FIFO lanes, exclusive fences, cancellation, model priority
  commands/                  Slash-command parsing, dispatch, middleware, adapters
    builtin/                 Command composition and help
    accountlinking/          Connect/disconnect adapters, not account infrastructure
    bootstrap/               First-administrator command
    globalmemory/ mcp/ memories/ session/ stop/ usermanagement/
  compaction/                Foreground/background model compactor and durable planner
    budget/                  Token estimates, input capacity, pressure and tail policy
  config/                    Environment loading, fixed policy, logging, sanitization
  database/                  SQLite opening, migrations, low-level account persistence
    migrations/              Immutable released SQL files
    maintenance/             Serialized maintenance worker
  gateway/
    routing/                 Transport-neutral admission and prompt construction
    runtime/                 Shared command/agent execution and delivery handling
    discord/ homeassistant/ imessage/
  identity/                  Dependency-light authenticated-principal contracts
  llm/                       Provider-neutral types and model gateway HTTP client
  mcp/                       Configuration, encryption, sessions, schemas, execution
  media/                     Shared image/video normalization and output attachments
  memory/                    Transactional user-memory/session/job/index storage
    policy/                  Pure evidence validation and activation policy
    extraction/              Private user-memory model calls and decoding
    formation/               Durable post-delivery formation worker
    global/                  Administrator-curated global-memory store and retrieval
    indexing/                Derived-index lifecycle worker
    memorytest/              Shared fixtures imported only by tests
  shared/                    Directory grouping only, not a Go package
    requestctx/              Request metadata, principal, images, exposure, staging
    lease/                   Renewable lease heartbeat
    invalidation/            In-process authorization/gateway-cache invalidation
  soul/                      Read-only system-prompt loader
  startup/                   Application assembly, ordered cleanup, startup output
  tools/
    names/                   All ten stable builtin tool-name constants
    exposure/                Request-local tool visibility
    governance/              Duplicate, per-tool, and request-wide limits
    registry/                Markdown schemas and builtin handler registration
    builtin/                 Tool adapters and tool-specific provider implementations
```

### Dependency Rules

- `internal/startup` is the composition root. It imports domain packages; domain packages must not import it. `main` should not assemble stores, workers, tools, or gateways.
- Shared account functionality belongs in `accounts`, not `commands/accountlinking`. User/global-memory storage belongs in `memory`, not builtin tool handlers. Commands and tools adapt their respective entry points to those services.
- Keep transaction-fenced candidate publication, job/outbox changes, account merges, and deletion together. Do not turn an atomic operation into independent service commits to achieve a directory split.
- Worker packages depend on stores, never the reverse. `memory` must not import `memory/formation` or `memory/indexing`; `database` must not import `database/maintenance`.
- Token-budget policy belongs under `compaction/budget`. The agent assembles prompts using that policy; `llm` owns wire token fields and provider usage telemetry. Budget estimators depend on LLM types, so the LLM client must not import the budget package back.
- Concrete gateways are composed by `gateway/bootstrap.go`. They use `gateway/routing` and `gateway/runtime` without importing the parent composition package. Neither `main` nor `startup` should import concrete gateways directly.
- `shared` is not a utility facade. Import `shared/requestctx`, `shared/lease`, or `shared/invalidation` directly. Do not add a generic `utils`, `common`, or forwarding package for unrelated functions.
- `tools/names` stays dependency-free. The builtin registry and MCP provider remain separate; the agent combines them into an advertised request catalog. Do not make builtin handlers import the parent `tools` composition package.
- Web and ComfyUI clients are currently tool-specific implementations under `tools/builtin`. Move functionality into a domain package when it has shared ownership, not simply because it is reusable in theory.
- `memory/memorytest` is test support, never a production dependency. Same-package memory tests use private local fixtures to avoid importing their parent through the test-support package.

### File And API Names

- Prefer responsibility-based files: `session_delivery.go`, `candidate_store.go`, `account_merge.go`, `replies.go`. Avoid dumping unrelated behavior into `store.go`, `types.go`, or `helpers.go`.
- `agent/agent.go` owns the main loop. Its context, compaction coordination, streaming, tools, tool history, and image retries are separate files in the same package; do not fragment the state machine into unnecessary services.
- `accounts/service.go` owns construction/lifecycle; `identity.go`, `moderation.go`, `identifiers.go`, and `challenges.go` own their named responsibilities.
- `llm/types.go` contains provider-neutral contracts; `llm/gateway_wire.go` contains private wire representations.
- `memory/formation_jobs.go` persists work; `memory/formation` runs it. Likewise, `memory/compaction_jobs.go` persists compaction jobs while `compaction` plans and executes them.
- Names must distinguish pending versus delivered, staging versus publication, paging versus complete results, and deactivation/retirement versus deletion. Keep `Tx` suffixes where callers supply the transaction.
- Prefer a named input struct for a multi-field write, as in `AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{...})`, rather than an expanding positional API or feature suffix chain.
- Use descriptive test and fixture names, not issue numbers or retired production API names. Release/version names remain appropriate for actual migration and artifact compatibility tests.

## Code Style And Change Discipline

These are requirements for new and changed code, not guarantees that every existing implementation conforms.

- Use `gofmt`. Group imports as standard library, third-party, then internal; use aliases only when they disambiguate package names or prevent shadowing.
- Prefer the smallest correct change. Keep cohesive code in one function/package unless extraction improves a real boundary or removes meaningful duplication. Do not add repository/service/interface layers merely for symmetry.
- Add doc comments to exported APIs. Comments should explain invariants, ownership, units, or non-obvious decisions, not restate assignments. Keep error paths and state transitions explicit.
- Use `context.Context` as the first argument to cancelable work and propagate cancellation. Detached contexts are reserved for deliberately independent worker lifetimes and bounded cleanup/durable bookkeeping; document why they are needed.
- Wrap errors with `%w` when the caller must retain the cause. Use typed/sentinel errors and `errors.Is`/`errors.As` for behavioral decisions rather than matching human-readable text.
- Only `main` calls `Fatal` or exits the process. Domain and startup APIs return errors after required cleanup. Report recoverable degradation with structured warnings; do not panic for routine runtime failures.
- Make transaction and lock ownership clear. Defer rollback, close rows/resources, check iteration errors, and keep tenant/source/lease predicates in the transaction that performs a mutation.
- Keep persisted units explicit: UTF-8 bytes, runes, tokens, durations, and row-selection bounds are not interchangeable. Do not truncate inside a UTF-8 sequence or split a conversation exchange to fit a prompt.
- Preserve model-visible names, JSON fields, error codes, log event/metric keys, commands, routes, environment variables, and persisted artifact formats during internal renames. Exported Go-field renames may change implicit JSON; inspect serialization before renaming.
- Do not add backward-compatibility aliases without a concrete persisted-data or external-consumer requirement. Do not delete active compatibility decoders just because new work uses another format.
- Keep unrelated user changes intact. Never commit credentials, local databases, generated media, or real transcript fixtures. Update `.env.example` with actual configuration changes and this file with contract changes.

## Test Contract

Tests must run without project secrets or live LLM, Discord, BlueBubbles, MCP, Brave, SearXNG, ComfyUI, or embedding services.

- Use fake model clients/transports, `httptest` servers, temporary files/databases, injected clocks, and explicit synthetic principals and credentials.
- Use `memory/memorytest` for external-package memory fixtures. Keep generation fencing and explicit pending-to-delivered transitions; do not introduce parallel SQL writers to bypass production publication. Direct SQL fixtures are appropriate for deliberate migration/corruption/invariant tests.
- Keep helpers in `_test.go` unless shared test packages require otherwise. Production code must not import test helpers or expose compatibility APIs solely to preserve old fixtures.
- Prefer channels over sleeps for lifecycle tests. Join fake goroutines, close stores, restore environment changes, and keep concurrent log captures synchronized.
- Configuration tests must not depend on a developer's `.env` or inherited secrets. Avoid printing complete config structs in failure messages.
- Startup tests use private per-call database-path, registry, and gateway seams. Do not start real listeners to test `startup.Run`.
- Test both callback-present and callback-absent model calls, pending/failed/delivered turns, stale leases, account-merge fences, fallback retrieval, and disabled tools when changing those paths.
- Retain failure and rollback coverage during refactors. Passing compilation alone does not establish SQL, wire, delivery, or authorization equivalence.

## Startup And Shutdown

`cmd/agent/main.go` loads config, calls the terminal-gated banner, creates the logger, registers interrupt/SIGTERM handling, and calls `startup.Run(ctx, cfg, log, stdout)`. It releases signal registration before final fatal logging.

`startup/app.go` validates required model settings and assembles components in this order:

1. LLM client, context budget, and soul loader.
2. User-memory, global-memory, MCP, and account database handles; MCP manager and account service.
3. Bootstrap command service; a process-local code and printed instructions are created only when no administrator exists.
4. Indexing and immediate-then-periodic maintenance workers.
5. Builtin registry, MCP provider, private memory extractor, formation service, and shared compactor/compaction service.
6. Agent, then broker workers, command service, invalidation bus, and enabled gateways.
7. Formation/compaction low-priority gates and worker starts, then gateway goroutines.

Cleanup is registered as resources are acquired and runs on both ordinary shutdown and partial initialization failure. The order is **maintenance, broker, formation, compaction, indexing, MCP clients, accounts DB, MCP DB, global-memory DB, user-memory DB**. It is intentionally not reverse acquisition order.

- Startup returns `startup.Error` with the original event, message, and cause only after cleanup. MCP close failures are warnings; database close errors are discarded here and do not replace initialization errors.
- Startup logs build metadata and initialization/cleanup phase boundaries. `app.shutdown.complete` follows every acquired resource's cleanup callback, including partial initialization failure, and reports cleanup duration and reason. It does not establish a gateway-stop/readiness contract.
- Signal cancellation triggers cleanup; it is not the parent of every worker context. Worker `Stop` methods must complete before their databases close.
- Pre-cancellation and explicit initialization-boundary checks return normally. Cancellation does not interrupt every initializer or override every simultaneous initialization error.
- Gateway `Start` failures are logged asynchronously. There is no readiness handshake or graceful gateway-stop interface. Real listeners may outlive `Run` until process exit; it is not a restartable in-process application API.
- `startup/banner.go` prints the fixed UTF-8 wordmark and URL in bright-magenta ANSI only to terminal stdout. Banner failures are ignored; the banner is not readiness.
- `startup/bootstrap.go` prints nonempty bootstrap instructions to the supplied writer even without a terminal. Codes are never structured-log fields. Claiming from authenticated Discord, iMessage, or Home Assistant is supported.

## Requests, Routing, And Accounts

Gateways normalize messages, attachments, replies, external identities, and conversation scope. `accounts` resolves canonical ownership; `identity.Principal` carries canonical user, external identity, gateway, and assurance. `shared/requestctx` propagates that principal, correlation metadata, current images, exposure state, and bounded foreground-memory staging.

`gateway/routing.Decide` chooses ignore, command, model submission, or direct fallback. `gateway/runtime.Execute` handles the admitted operation and gateway-specific responder.

- Private conversations do not require mentions. Ordinary group messages require a mention or a recognized reply to Oswald. Group slash-command attempts require a mention even when replying to Oswald.
- Command handlers do not receive private/group scope. `/connect`, `/bootstrap`, and other admitted authenticated commands can run in groups; codes and responses may be visible there.
- Reply enrichment can include quoted text, replied-to images within remaining slots, unsupported labels, or unavailable-message markers. Incomplete Discord references or uncached iMessage replies can be ignored during preflight before later lookup occurs; invocation through every incomplete reply is not guaranteed.
- Shared routing checks for empty assembled input, including reply and unsupported-file notes. Home Assistant rejects blank text earlier; iMessage ignores payloads without text or attachments.
- Runtime requires authentication and checks bans before admitted commands/model work. Ignore and direct empty-fallback paths return earlier. A ban is not a universal execution-time recheck or cancellation of already-admitted work.
- The command dispatcher validates principals but does not enforce `AdminOnly` metadata by itself. Builtin assembly installs admin middleware; `/mcp global` and `/stop all` check authorization explicitly.
- Admin authorization re-resolves the external account. Account mutations and memory commands have additional ownership/fencing rules; do not assume every handler refreshes ownership identically. `/reset` and user MCP commands use the carried canonical ID.

### Broker And Command Scheduling

`broker` schedules normal operations in FIFO lanes keyed by canonical user and session. Only the lane head occupies a worker. The outstanding-work limit is ten plus the effective worker count, including active and queued scheduled work; nonpositive worker configuration becomes one. Agent saturation produces a fallback, while scheduled commands can be rejected with an error response rather than blocking indefinitely.

- User-exclusive command fences protect account-wide operations. Connect confirmation can fence both participants; delete-user fences its target. Fences remain held through response delivery and invalidation, not just handler execution.
- Accepted commands use independent execution contexts and drain during shutdown. Agent work is cancelable through broker lifecycle contexts.
- `/stop` runs out of band and cancels only the current session's active agent request, preserving queued prompts. Admin `/stop all` cancels active and queued agent work, not commands or durable background jobs.
- Formation and compaction share one low-priority model permit. It is admitted only with no outstanding foreground work; accepted foreground work cancels its active context so the durable job can refund/defer.

### Account And Command Contracts

User commands are `/help`, `/connect`, `/disconnect`, `/reset`, `/stop`, `/memories`, `/bootstrap`, and `/mcp`. Administrative operations include `/stop all`, `/users`, `/user`, `/admin`, `/unadmin`, `/ban`, `/unban`, `/deleteuser`, `/global-memory`, and `/mcp global`. See command definitions and README for exact argument syntax.

- Account-link challenges last ten minutes. Store hashes, expiry, and consumption/replay identity, not plaintext codes. A new outgoing challenge invalidates the user's prior active one.
- The initiator's canonical account remains the owner after a merge; admin status is preserved if either participant is admin. Reject banned participants, conflicting accounts for the same gateway, and conflicting user MCP names. Frozen session-profile selection follows the separate generation/snapshot rules below.
- Confirmation atomically moves accounts, memory/session state, jobs, summaries, and reencrypted MCP ownership before deleting the losing user. Verify loser-owned rows are absent before commit. Same-confirmer replay can return the prior result without another merge.
- `/disconnect` cannot remove the final account. An administrator cannot unadmin, ban, or delete themselves.
- `/bootstrap` promotes the submitting account's currently resolved owner only while no administrator exists. Its process-local code is consumed after a successful update; restart replaces it only while no admin exists.
- `/memories list` exports all active, unexpired memories, not just the model list limit. It uses UTF-8-safe multipart attachments and fails rather than returning a partial export when the output limit is exceeded.
- `/memories forget <id>` requires a positive decimal ID and physically deletes the memory, candidates, affected profile references, and physical/queued derived-index state. Source conversation and summaries remain.
- `/memories forget all` uses `ResetUserDataPreservingAccount`: delete learned memory, candidates, user MCP config, session turns/summaries/jobs, derived serving state, and account-link challenges; reset session generations while retaining account identity, linked accounts, moderation, speaker intro, and high-water bookkeeping.
- `/reset` advances one tenant/session generation, deletes its turns, summaries, formation/compaction work, and binds the latest profile. Durable user facts remain.
- Invalidation clears relevant gateway caches. Disconnect/delete-user can request closure of matching Home Assistant sockets; forget-all does not currently request connection closure.

## Agent And Context Management

`Agent.Process` receives `agent.Request` and returns `agent.Response`. Prompt assembly is in `agent/context.go`; tool/catalog/history helpers, image retry, streaming, and active-loop compaction coordination are separate files in that package.

1. Load the soul fresh, add trusted gateway instructions, resolve the frozen tenant profile, and retrieve tenant-scoped durable recall.
2. Load the latest summary and delivered recent exchanges for the session generation. Independently query recent successful MCP names for continuity.
3. Assemble required deployment policy, profile, current text/images, and advertised tool cost. Reserve up to two newest complete exchanges within the recent-tail allowance, then a summary if it fits, then whole recall records and additional recent exchanges.
4. Call the model; authorize calls against that iteration's exact catalog, execute allowed tools serially, append one correlated result for every declared call, and repeat.
5. Between complete tool rounds, compact when pressure requires it. On a global tool ceiling, finish with a tools-disabled model call.
6. Persist the final exchange, bounded native history, staged memory artifact, and pressure snapshot as pending delivery. The shared runtime activates post-delivery work only after the responder succeeds.

### Budget And Authority

- `compaction/budget` owns token estimation, output/safety reserves, the 70% compaction trigger, and recent-tail capacity. No model-metadata discovery is performed.
- Default context window is 32,768 tokens; default output reserve is 8,192; safety margin is 256. Nonpositive model-limit configuration selects these fallbacks. Tools and images are estimated within each actual request, not a fixed tool reserve.
- The recent-tail allowance is 25% of usable input, bounded to 2,000-8,000 tokens and never more than available input. This is the exchange allowance, not a combined summary-plus-tail allocation. Under pressure, a selected tail can take priority over the optional durable summary.
- Recall records are indivisible; a non-fitting record can be skipped while later records are considered. Additional history stops at the first complete exchange that cannot fit even after native tool history is omitted.
- Soul/gateway deployment policy is system-authority content. The profile is subordinate user-authority context; summaries, recalled facts, and historical tool results are explicitly untrusted reference data. They cannot grant authorization or tool access.
- Current images are not replayed into later requests or persisted as image bytes. Stored user text uses an attachment marker; reply context and injected recall are stripped from durable conversation/query text.
- Historical exchanges replay native tool calls and correlated results only when current tool availability and history policy allow it and the trace fits. Otherwise retain the exact user/final-assistant pair.
- Required context that exceeds input capacity is retained and logged; optional context is omitted. Provider usage is telemetry, not the compaction policy input.

### Failure And Persistence Behavior

- Oswald has no overall LLM generation deadline. Broker cancellation, transport errors, tool ceilings, and the independently configured gateway/provider timeout bound work.
- Each image-retry sequence allows **five total model attempts** for recognized Ollama image-runner resource failures, progressively reducing current images. This is not a request-wide submission limit; later tool rounds and corrective calls can begin another sequence. Exhaustion returns a fixed image-size fallback; unrelated errors do not use this path.
- The current Ollama/Qwen tool-parser workaround retries an identical request once. Repeated recognized parser failure returns a fallback. Empty visible responses have a separate tools-disabled corrective retry.
- A provider context-length failure can force compaction. Non-cancellation context failures use a deterministic partial-completion response that warns completed actions were not undone and retains permitted artifacts. Cancellation exits without publishing a completed turn.
- Ordinary session append failures may log and still return the answer. If a memory save was staged, unavailable or failed persistence returns an error instead of delivering an unpersisted save claim.
- Pending or failed-delivery turns cannot enter durable summaries, transcript search, or background extraction. Late successful delivery can clear a timeout failure and restore eligibility.

## Memory And Compaction

| Layer | Canonical Location | Write Authority |
| --- | --- | --- |
| Soul | `data/memory/soul/soul.md` | Operator filesystem access only |
| Global facts about Oswald | `global_memories` | Administrator commands |
| Durable user facts | `memory_entries` and candidate evidence | Post-delivery formation |
| Conversation continuity | `sessions`, `session_turns`, `session_summaries` | Agent persistence and validated compaction |

### Durable User Memory

- Ownership is the canonical user, shared across linked accounts. Addressed group turns deliberately use the sender's private memory; groups do not create a shared memory tenant.
- Categories are `identity`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, and `notes`.
- `user_memory_save` stages at most five current-turn observations; it does not publish canonical memory inline. Evidence is model-assessed as a direct statement or inference with finite confidence from 0 to 1.
- Background pattern extraction receives a frozen window of two to eight delivered user turns. It returns at most three patterns, each supported by two to five distinct whole-turn observations including the newest anchor. Repetition provides corroboration, not a fixed confidence increment.
- Current formation validates structure, source membership, exact evidence, category-compatible claim identity, tenant ownership, delivery, and leases. It does not apply regex semantic rejection of credentials, directives, negations, quotations, or third-party content. Do not document those classes as categorically unsavable.
- Confidence below 0.35 remains proposed evidence; approved evidence can publish or reinforce a canonical claim. An approved candidate without a published memory link can be blocked by a stronger conflicting claim. Sensitivity is retained independently and does not prompt conversational confirmation.
- Stable `(claim_slot, claim_value)` identity consolidates equivalent evidence. Same-turn reconciliation does not double-count. Confidence may rise to the maximum submitted assessment; stronger authority/confidence can improve wording or supersede conflicts. `.fact` slots are multi-valued.
- `provenance_type` determines serving authority: `user_statement` is user-direct, `model_inference` is model-derived, otherwise unknown. Inference is labeled possible below 0.5, likely below 0.8, and high-confidence at or above 0.8.
- Candidate insertion/reconciliation, publication/reinforcement, supersession, profile advancement, and index outbox changes commit together under lease/source fencing. Derived FTS/vector writes are asynchronous.

### Profiles And Retrieval

- Profiles compile eligible active, unexpired long-term facts into at most 2,000 **bytes**. Identity/communication facts need confidence at least 0.8; durable preferences/environment need at least 0.9. Other categories are not profile facts.
- Normal publication does not refresh an already-bound profile. New, expired, or reset sessions bind the latest version. Explicit deletion recompiles affected snapshots; this can include other newly eligible facts.
- Account merge preserves frozen snapshots, remaps generation collisions and profile versions, and retains high-water marks. It is not an automatic refresh of every active conversation to the unified profile.
- `sessions` stores the frozen text, digest, renderer, speaker intro, source memory IDs, and version/generation high-water. Fact count comes from the checked source-ID JSON array; byte size comes from frozen UTF-8 text.
- Automatic recall combines lexical FTS5 and optional semantic sqlite-vec results, then applies relevance thresholds, confidence/importance/recency/authority ranking, duplicate suppression, diversity, and output limits. Failure of one channel does not relax tenant filtering.
- `user_memory_search` uses the hybrid engine for deeper recall; `user_memory_list` lists active facts. `session_transcript_search` is lexical search over complete delivered exchanges in the current authenticated session's active generation, not other/reset/expired sessions. Its defaults are five results, maximum ten, within a 12,000-byte result-accounting cap.
- Global memory is not automatically injected. `global_memory_search` retrieves administrator-curated deployment/implementation facts and returns bounded newline-delimited JSON. Both derived channels may fall back to a bounded canonical scan. Global additions normalize/deduplicate and enforce 1,000 runes; they do not filter credentials or instruction-like content.

### Durable Formation

`memory/formation` orchestrates jobs; `memory/formation_jobs.go` and `memory/candidate_store.go` own transactional state.

- Model-backed formation has **three durable provider-submission credits**, reserved immediately before invocation. Successful submissions consume credit too. Operational failures and the one invalid-output corrective retry share that budget.
- Current extraction forces one `user_memory_pattern_extract` call, `tool_choice = "required"`, no parallel tool calls, temperature 0, and the resolved output-token limit. Invalid output gets at most one reason-aware corrective retry; prior raw model output is not replayed.
- Persist the first valid decoded artifact for idempotent replay. Replaying it does not invoke the model. Local `agent_save` jobs do not consume provider credit and retain bounded storage retry behavior.
- Renewable exact-token leases begin at five minutes. Publication requires a live exact lease; retry/skip release paths retain exact-token ownership checks even after natural expiry.
- Intentional foreground preemption refunds the reserved submission, restores the claimed attempt, and durably defers work regardless of remote acceptance. Startup backfill considers missing jobs only for eligible delivered turns from the preceding 24 hours.

### Session Compaction

`compaction.NewLLMCompactor` supplies the shared model-backed compactor. `compaction/service.go` plans/runs durable jobs; `agent/compaction.go` installs request-local checkpoints; `memory` stores jobs, summaries, and delivery state.

- `AppendPendingSessionTurn(ctx, SessionTurnWrite)` writes a generation-fenced exchange and immutable history, staged memory, pressure, and outbox artifacts. Pressure requires nonnegative tokens, a positive limit, and a nonblank version. Persistence is not delivery acknowledgement.
- At 70% estimated usable-input pressure, durable planning pins a campaign target through the newest eligible delivered exchange. Jobs cover at most 64 exchanges; successful partial checkpoints continue the campaign even if pressure falls.
- Each job pins model/generator contracts. One queued/running/retry job per tenant/session/generation prevents overlap; failed contracts and uncompactable complete-exchange receipts suppress repeated work until the contract or scope changes.
- `PageDeliveredSessionTurnsAfter` pages ascending IDs across pending gaps for foreground history. Advance its exclusive boundary to the last returned ID. `CompactionWindowAfter` instead respects pending-delivery barriers and reports the eligible total/newest ID independently of page size.
- Durable jobs have four provider submissions and up to three structured-output corrective retries within those four credits. Permanent provider 4xx failures skip immediately except 408, 425, and 429. Foreground compaction also shares four submissions across its chunks/retries.
- Compaction uses a silent synchronous stream, one required `session_summary_save` tool call, no parallel calls, temperature 0, and the resolved output limit. Provider schema omits grammar-expensive cardinality/string-length constraints; local validation still enforces limits.
- A summary contains narrative, open tasks, commitments, entities, decisions, topic tags, covered range, and ordered source IDs. Narrative is bounded to 8,000 runes; arrays to 50 items each, items to 1,000 runes, aggregate structured text to 16,000 runes, and encoded artifact to 40,000 bytes.
- Foreground compaction includes delivered post-checkpoint exchanges and completed active tool rounds, using live model-visible arguments/results rather than durable-history truncation. Reasoning and attachment bytes are excluded. Install a checkpoint only after validation; keep the old context on failure.
- Durable summaries do not delete covered transcripts or publish user memories. Active-generation transcripts remain searchable after compaction.

### Persisted Compatibility

Retained `formation-v4` jobs/artifacts still use their single-turn decoder and stricter legacy evidence policy. Current pattern and foreground behavior must not be inferred from those legacy rejection rules.

New compaction output requires an empty `candidates` array. Persisted summary artifacts retain a legacy candidate field: decoding still validates structural and size bounds, but summary publication neither evaluates those candidates as new user evidence nor publishes them. Keep these decoders and the existing artifact version/JSON contracts while stored data can require them.

## SQLite, Indexing, And Retention

The canonical database is `config.DefaultDatabasePath`, currently `data/database/oswald.db` relative to the working directory. Accounts, MCP, global memory, and user memory open separate handles to it. Initialization is serialized by a process schema mutex.

- Permanent SQL migrations are embedded, semantically ordered `vMAJOR.MINOR.PATCH.sql` files. Current history is `v4.0.0` through `v4.0.9`, ten ledger rows. Sequence numbers are application order, not release versions; SHA-256 protects release name plus SQL.
- Accept empty databases or an exact applied prefix of that registry. Reject nonempty ledgerless/development schemas and checksum drift without modifying canonical schema/data. There is no pre-v4 importer.
- Apply missing migrations on one connection in one `BEGIN IMMEDIATE` transaction with foreign-key actions temporarily disabled, check foreign keys before commit, and restore enforcement afterward. Never edit a released migration.
- `durable_jobs` contains typed formation, compaction, and derived-index work. Job-kind checks and tenant/source/lease predicates are part of the persistence contract, not redundant metadata.
- Canonical state remains authoritative when indexes are absent. Derived kinds are `memory_fts`, `transcript_fts`, `memory_vector`, `global_memory_fts`, and `global_memory_vector`.
- Canonical mutations enqueue outbox work transactionally. Indexing applies it to relevant live/building revisions, validates canonical version and ownership, and retries without weakening filters.
- Rebuild into generated shadow tables, validate physical dimension/schema/model, exact canonical and valid indexed counts, tenant joins, memory eligibility, and delivered active-generation transcript eligibility, then atomically switch the live pointer. Failed shadows do not replace working indexes.
- Generated names must match their recorded kind/revision before publication or cleanup. Retained revision metadata preserves high-water and prevents name reuse.
- Indexing reconciles at startup and polls every 30 seconds plus mutation wakeups. During model replacement, semantic queries use the old live model until publication; that model must remain accessible. Embedding dimension is cached after the first successful probe until restart.

`config.DefaultRetentionPolicy()` supplies these code-owned values; environment overrides are not supported. Tests can inject policies.

| Policy | Value |
| --- | --- |
| Retired/failed physical index-table retention | 168 hours |
| Session inactivity | 24 hours |
| Pending delivery timeout | 15 minutes |
| Successful/skipped job history | 168 hours |
| Dead-job and unpublished-candidate history | 720 hours |
| Account-link challenge cleanup grace | 24 hours after expiry |
| Maintenance schedule | Immediately, then hourly |
| Minimum database optimize interval | 24 hours |
| Rows selected per maintenance operation | 100 |

- Expired user memories are hidden on reads. Maintenance marks due active rows expired, blanks their statement/claim identity, retains the canonical row and remaining metadata, and queues index deletion. Explicit forget is physical deletion; expiry is not equivalent.
- Session-serving queries require a matching active, unexpired generation. Delivered turns remain usable for that session lifetime even if the turn's own expiry has passed. Expiry cleanup removes artifacts but retains inactive session bookkeeping/high-water rows.
- Inactive compaction work can first be retired/skipped and detached from summaries, with terminal retention applied later. Do not equate every cleanup count with immediate row deletion.
- Keep active-generation failed-contract compaction receipts and one newest successful upsert receipt per still-eligible canonical entity. They prevent repeated failing compaction and needless reindexing, respectively.
- Maintenance is serialized but not one atomic sweep: expiry cleanup, further canonical retention, and derived/database hygiene have separate commit/error boundaries. Later failure does not roll back earlier committed cleanup. Batch size bounds selected rows per operation, not a whole-category or whole-sweep deletion total.
- SQLite uses foreign keys, `secure_delete=ON`, WAL, `synchronous=NORMAL`, a five-second busy timeout, immediate write locks, and a 1,000-page automatic WAL checkpoint. Sweeps perform a passive checkpoint, `incremental_vacuum(100)` only if already in incremental-vacuum mode, and `PRAGMA optimize` when due.

### Backups And Container Paths

Use SQLite online `.backup`, or stop Oswald before copying the database together with any WAL/SHM companions. A live copy of the main file alone is unsafe. Keep the exact MCP encryption key separately. Restore while stopped, remove stale destination WAL/SHM files, and require `PRAGMA integrity_check` to return `ok` plus an empty `PRAGMA foreign_key_check` before restart. External backups and logs need independent retention/access controls; application deletion cannot erase their copies.

The Docker working directory is `/home/oswald-ai/`, so the default database resolves to `/home/oswald-ai/data/database/oswald.db`. The image also creates `/data/database`, but that is not the configured application path. Mount/persist the path actually used. `EXPOSE 8000` neither configures a gateway nor publishes a host port. The image runs as the nonroot `oswald-ai` user and includes `ffmpeg` and SQLite runtime tools.

## Tools And MCP

Builtin names live in `tools/names/names.go`; its contract test pins all ten strings and their exact correspondence with loaded Markdown schema names. Private formation/compaction tools are not builtin catalog entries.

| Tool | Enablement | Execution / Failure / Unproductive Limits | Durable History |
| --- | --- | --- | --- |
| `time.current` | Always | 0 / 0 / 0 | Full |
| `user_memory_save` | Always | 2 / 0 / 0 | Metadata |
| `user_memory_search` | Always | 0 / 0 / 0 | Full |
| `user_memory_list` | Always | 0 / 0 / 0 | Full |
| `session_transcript_search` | Always | 0 / 0 / 0 | Full |
| `global_memory_search` | Always | 0 / 0 / 0 | Full |
| `web.search` | Brave or SearXNG configured | 0 / 2 / 2 | Full |
| `web.fetch` | Same enablement as search | 4 / 2 / 2 | Metadata |
| `comfyui.text_to_image` | ComfyUI configured; hidden from Home Assistant | 0 / 0 / 0 | Metadata |
| `comfyui.image_to_image` | Same, plus a current image | 0 / 0 / 0 | Metadata |

Zero disables a per-tool guard; it does not bypass global limits. Full history is bounded and searchable; metadata-only fetch/save/image results are not persisted as full model/tool content. Default per-call durable-history bounds are 16 KiB of arguments and 16,000 result runes; the complete trace has additional aggregate bounds.

- `governance.DefaultGlobalPolicy()` caps requests at 50 actual handler executions and 30 model responses containing tool calls. There is no request-wide consecutive-failure guard.
- Authorize against the exact catalog advertised for that iteration. Discovery cannot authorize another call in the same model-emitted batch. Complete every declared call with a correlated result, including blocked calls, before finishing with tools disabled.
- Duplicate detection hashes the name and canonical normalized arguments. Successful/unproductive calls retain fingerprints; execution errors release them for exact retry. Per-tool exhaustion hides only that tool.
- `user_memory_save` has two executions and a five-candidate request-wide staging cap. The model is instructed to use the second call for retryable corrections; runtime does not enforce that every second call is semantically a correction.
- Current time is not injected automatically: use `time.current` when needed. It accepts IANA zones/UTC and rejects host-dependent `Local`.
- The registry loads schemas and owns builtin handlers/policies/disabled-name reservations. Invalid individual Markdown specs are logged/skipped; required handler registration can then fail startup. MCP tools are supplied separately and combined by the agent.

### MCP Configuration And Exposure

- MCP is generic; GitHub-specific capabilities come from a configured remote server, not a dedicated GitHub integration in Oswald.
- User servers belong to one canonical user. Global servers are visible to all eligible users and use shared configured remote credentials, not per-user delegated OAuth. Global management commands require administrator authorization; remote writes are not admin-only merely because the server is global.
- `/mcp add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>] <description>` and `/mcp global add ...` save/update the complete configuration and invalidate the cached connection.
- Names are 2-40 lowercase ASCII characters, starting with a letter and continuing with letters, digits, or underscores; `soul` is reserved. Descriptions are trimmed, 1-500 runes, and reject controls. The stored description is used unchanged for `<server>.tools`; command tokenization may normalize input whitespace.
- `mcp_servers` stores plaintext descriptions/metadata and AES-256-GCM-encrypted URLs and headers. A fresh nonce is used per encryption; associated data binds scope, owner, server name, and field. Account merge reencrypts for the winner. The key is required even without configured servers.
- Servers without descriptions are hidden until updated. Successful MCP tools from the latest four delivered exchanges can be pre-exposed if still available to the user, independently of the summary boundary.
- Remote sessions are not opened at application startup. Discovery, testing, execution, and catalog assembly can connect lazily; assembling exposed tools may connect to all enabled described servers visible to that user.
- Discovery exposes matching names request-locally, then bounds its result listing. A large listing can omit names already exposed. Both discovery and remote-result envelopes are bounded to 16,000 runes after receipt/flattening, not by an equivalent HTTP-body cap.
- Remote read and write tools are eligible without annotation-based mutation filtering, subject to reserved-name, identifier, and simplified-schema checks. Remote `tools` is reserved. Fully qualified names must fit 64 bytes.
- MCP schema projection is not full JSON Schema support: it forwards compatible top-level object parameters, required flags, bounded descriptions, and safe string enums. Unsupported identifiers/types, malformed schemas, and non-string enums can hide tools; nested schemas and combinators are not faithfully preserved.
- Only `streamable_http` connection handling is implemented. Stored `sse` configuration is rejected when connection is attempted. The HTTP client has a 30-second timeout; initial session connection has a separate 30-second context deadline, not one total discovery/execution deadline. Non-text results may be JSON-flattened, not converted to gateway attachments.

**MCP network limitation:** URL validation requires HTTPS without userinfo and checks resolved addresses at save/connection setup against private, loopback, link-local, multicast, and unspecified classes. It does not provide `web.fetch`'s full special-range filtering, validated-IP dialing, proxy exclusion, or redirect revalidation. The default HTTP transport can resolve again/use proxies, and configured headers are reapplied to redirected requests. Treat MCP endpoints and catalogs as trusted operator/user configuration; do not claim DNS-rebinding or cross-origin credential-forwarding protection is implemented.

### Web Providers

`websearch/normalize.go` is provider-neutral. Queries are limited to 400 runes/50 words; provider bodies to 2 MiB; the first 50 source-ordered candidates are normalized into at most eight results, at most two per hostname, within a 16 KiB model envelope. Remove known tracking parameters, fragments, and duplicate URLs. Result URLs/text are untrusted; search normalization is not the direct-fetch DNS security boundary.

- Brave uses `POST https://api.search.brave.com/res/v1/llm/context`, API version `2026-07-31`, US English, spellcheck, SafeSearch off, balanced relevance, eight requested URLs, and an approximate 3,072-token context budget. The shared client allows 50 attempts per rolling second and a 30-second per-attempt timeout.
- Brave retries once only for explicit 408, eligible short-window 429, or selected 5xx responses. Ambiguous transport failures/timeouts, long quota windows, and malformed/oversized successes are not retried because a metered request may already have completed. Redirects are not followed.
- SearXNG uses the configured HTTP(S) base path plus `/search`, JSON, `en-US`, general category, and first page. Engine selection/weighting is deployment-owned. It has an eight-second attempt timeout, at most one retry, and same-origin-only redirects; private SearXNG deployment addresses are permitted.
- With both providers, Brave is primary; SearXNG runs after failure or no usable result. Failure makes fallback output degraded; clean emptiness does not. Partial engine failures and truncation are reflected in bounded results, not raw backend errors.
- Search queries, URLs, provider bodies, rate-limit headers, and credentials must not be logged. Bounded successful search arguments/results can still enter full session tool history.

`web.fetch` accepts public HTTP on port 80 or HTTPS on port 443. It validates every DNS answer, dials an exact validated IP, disables environment proxies and connection reuse, rejects userinfo/secret-like query keys/local or special destinations, and revalidates up to three redirects without HTTPS downgrade.

Fetch has a 15-second overall deadline, bounded headers, a 2 MiB direct decoded-body cap, and a 16 KiB model envelope. It extracts HTML/XHTML, plain text, and JSON; recognized public X/Twitter status URLs try the public oEmbed endpoint first with guarded direct fallback. It cannot render JavaScript, bypass authentication/challenges, or extract PDF/binary media. Arguments/results use metadata-only durable history.

### ComfyUI

ComfyUI handlers accept positive/negative prompts, each bounded to 2,000 runes, and image-to-image uses the first current image. For the existing gateways, tools are hidden from Home Assistant; the implementation currently checks the gateway name rather than a generic attachment-capability interface.

- Workflows are operator-owned templates subject to fixed graph/model validation, not arbitrary API graphs. Current templates require `dreamshaper_8.safetensors`; image-to-image pins `dpmpp_2m` and `karras`.
- Supported sizes are multiples of 64 between 64 and 768, within 768x512 or 512x768; steps 1-25 and CFG 1-10. Text-to-image uses batch one/denoise one; image-to-image denoise is 0.1-0.9. Runtime replaces prompts, a fresh random seed, and the current image reference.
- Downloaded outputs accept PNG/JPEG/GIF/WebP, at most 8 MiB, with each dimension positive and at most 8,192 pixels. Validation uses MIME checks and `image.DecodeConfig`, not full image decoding.
- One client serializes upload, submission, polling, output download, detached `/free` cleanup, and permit release. The configured generation timeout starts after permit acquisition; permit waiting remains caller-cancelable. Cleanup has a separate ten-second bound and may mark a valid output degraded on failure.
- `/free` unloads models globally, so require a dedicated or exclusively serialized ComfyUI instance. Generated output uses ephemeral Oswald attachments, not model-visible bytes or durable tool history; this does not imply files are deleted from ComfyUI's storage.

## Gateways And Media

| Gateway | Transport And Identity | Session Key |
| --- | --- | --- |
| Discord | Reconnecting Gateway WebSocket plus REST; Discord author identity | DM: `discord:dm:<author-id>`; guild/thread: `discord:<channel-id>:<author-id>` |
| iMessage | Authenticated `/bluebubbles/webhook` and BlueBubbles REST; normalized phone/email handle | DM: `imessage:dm:<sender-id>`; group: `imessage:<chat-guid>:<sender-id>` |
| Home Assistant | Bearer-authenticated `/homeassistant/ws`; trusted HA service asserts user ID | `homeassistant:<ha-user-id>:<conversation-id>` |

- Discord ignores bots, maintains heartbeat/resume/reconnection, resolves mentions, downloads attachments, and reconstructs replies. Its stream state machine displays thinking/tool/compaction activity, then cursor previews and finalized chunks under the 2,000-unit message limit. Authoritative final delivery reconciles streamed messages. Tool results remain hidden; builtin status exposes purpose-specific fields and MCP primitive arguments are bounded/secret-key-filtered.
- iMessage validates the BlueBubbles password through accepted password/guid query parameters or `x-password`, `x-guid`, `x-bluebubbles-guid` headers. It caches contacts/reply metadata, sends typing indicators, and delivers only the final response with a fallback send method. Cross-session replies provide quoted context, not a switch to another sender's session.
- Home Assistant requires exactly one bearer Authorization header, rejects Origin headers, sends `ready` with protocol version 1, reads one strict JSON text request, executes it, and closes. Multiple connections per user are possible. Unknown fields, binary input, anonymous users, missing conversation IDs, and blank text are rejected.
- HA incoming frames are bounded to 128 KiB with a 15-second first-message deadline. It streams correlated thinking/content/tool-call/tool-result frames and attempts at most one terminal result/error; disconnects/read/write failures can prevent delivery. Disconnect is not wired to foreground cancellation. Agent status chunks are suppressed; text command attachments are returned inline, while binary/image attachments are unsupported.

### Media Bounds

| Resource | Bound / Behavior |
| --- | --- |
| Current-turn images | At most four |
| Encoded source image | At most 10 MiB each |
| Accepted source formats | JPEG, PNG, GIF, WebP, HEIC/HEIF, including sequence MIME variants |
| Normalized input | JPEG or PNG; longest edge at most 2,560 pixels; encoded payload at most 280 KiB before base64 |
| Animated GIF | Sampled contact-sheet image |
| Discord GIFV | 10 MiB source payload, 20-second extraction deadline, up to four sampled frames via ffmpeg/ffprobe, with static-preview fallback |
| Output attachments | At most 8 MiB each, ten files, 80 MiB total |

Validate downloaded bytes using metadata, MIME/sniffing, and format signatures. Preserve transparency with PNG; otherwise normalize to JPEG. Unsupported or unusable attachments become bounded prompt notes rather than raw binary model input. Output filenames must be basenames without separators/controls, no more than 255 bytes, and unique within the response.

Source decoding precedes resizing, and animated GIF uses full animation decoding. Encoded-byte and output-dimension limits do **not** establish a strict peak decoded-memory or animation-frame bound. General files, audio, and arbitrary video inputs are not supported.

## Model Gateway Transport

`llm/gateway.go` maps provider-neutral types through `gateway_wire.go`. Chat transport follows the request's streaming setting, not whether tools are present:

| Request | Transport |
| --- | --- |
| Foreground with stream callback: Discord and Home Assistant | Synchronous streaming `POST /v1/chat/completions` |
| Foreground without stream callback: iMessage | `POST /v1/async/chat/completions`, authenticated status polling |
| Private extraction and compaction | Silent synchronous chat stream |
| Embeddings | `POST /v1/async/embeddings`, authenticated status polling |

Tool rounds and final tools-disabled calls retain the foreground stream setting. Silent streams allow foreground priority to close the active background HTTP request immediately.

- The gateway must support the Bifrost async contract and have a Logs Store configured for async routes. Polling defaults to one second; Oswald does not set a total LLM-client timeout.
- Async IDs are process-local, not persisted. Cancellation/restart after submission may leave remote jobs running until completion or the provider's independently configured timeout; no async cancellation endpoint is implemented here.
- Current-turn images use OpenAI-compatible image URL content blocks. Provider-reported thinking, content, usage, and finish reasons are mapped separately.
- `MODEL_MAX_OUTPUT_TOKENS` reserves foreground response capacity but does not send a foreground `max_tokens` cap. Private extraction/compaction send the resolved value as `max_tokens`.

## Environment Configuration

These are the 22 application variables loaded by `config.Load`. Defaults below are code defaults; explicitly empty strings generally differ from unset values.

| Variable | Default / Purpose |
| --- | --- |
| `HOME_ASSISTANT_AUTH_TOKEN` | Empty; at least 32 bytes after trimming whitespace when enabling HA |
| `HOME_ASSISTANT_LISTEN_PORT` | Empty; valid port required with HA token |
| `BLUEBUBBLES_LISTEN_PORT` | Empty; valid webhook port |
| `BLUEBUBBLES_URL` | Empty; absolute HTTP(S) base without credentials/query/fragment |
| `BLUEBUBBLES_PASSWORD` | Empty; webhook and API credential |
| `DISCORD_TOKEN` | Empty; enables Discord when nonblank |
| `MCP_CONFIG_ENCRYPTION_KEY` | Required at startup; base64-encoded or raw 32-byte AES key |
| `LLM_GATEWAY_URL` | `http://localhost:8080` when unset |
| `LLM_GATEWAY_MODEL` | Required nonempty route/model name |
| `LLM_GATEWAY_EMBEDDING_MODEL` | Empty disables semantic indexing/retrieval |
| `LLM_GATEWAY_API_KEY` | Optional bearer authentication |
| `LLM_GATEWAY_VIRTUAL_KEY` | Optional `x-bf-vk` routing header |
| `MODEL_CONTEXT_WINDOW` | 0 selects budget fallback |
| `MODEL_MAX_OUTPUT_TOKENS` | 0 selects output-reserve/private-call fallback |
| `BRAVE_API_KEY` | Empty disables Brave |
| `SEARXNG_URL` | Empty disables SearXNG |
| `COMFYUI_URL` | Empty disables image generation |
| `COMFYUI_TEXT_TO_IMAGE_WORKFLOW` | `data/workflows/comfyui/text-to-image-basic.json` |
| `COMFYUI_IMAGE_TO_IMAGE_WORKFLOW` | `data/workflows/comfyui/image-to-image-basic.json` |
| `COMFYUI_GENERATION_TIMEOUT` | Positive Go duration; default 2m |
| `WORKER_POOL_SIZE` | 1; nonpositive values normalized to one by broker |
| `LOG_LEVEL` | `info`; unknown values fall back to info |

- Invalid/incomplete gateway settings disable that gateway; startup fails if none are configured correctly. Ports must be integers from 1 through 65535.
- Invalid/empty integer text uses parser fallbacks. An explicitly empty/invalid/nonpositive ComfyUI duration fails config loading even if image tools are disabled. Malformed nonempty ComfyUI URLs fail config loading; malformed nonempty SearXNG URLs fail tool initialization.
- `.env.example` currently supplies HA/BlueBubbles ports and a nonempty ComfyUI URL as deployment examples. Copying that URL opts into ComfyUI; it is not the empty code default.
- Retention, maintenance, global tool limits, database/soul/schema paths, and per-tool limits are code-owned. Do not document retired environment overrides as supported. Standard-library environment behavior, such as proxies on default HTTP transports, is separate from this inventory.

## Structured Logging

The issue126 monitoring contract applies to all code and future contributions: operational data must be available at the default INFO threshold, even when no current dashboard or immediate consumer needs it. Instrument meaningful operation boundaries, outcomes, counts, latency, usage, and health rather than every line or payload. DEBUG may add safe diagnostics, but must not be the only source of required monitoring. The implementation details below describe current behavior; the contributor rules also govern new work.

Production logs are single-line JSON on stderr. Ingest stderr only. Human-readable banner/bootstrap output is stdout and can contain a bootstrap secret; exclude it from ordinary log ingestion, or apply a separate secret-safe filter if the collector cannot separate streams. Container TTYs merge streams and expose ANSI output. No additional application environment variables or database schema are required for this logging contract.

### Schema And Safety

`internal/config/logging.go`, `logging_fields.go`, and `logging_errors.go` define the output boundary. Every record has `ts`, `level`, `service`, `log_type`, `component`, `event`, `msg`, `log_schema_version`, `instance_id`, and `record_kind`. Schema version is `1`, service is `oswald-ai`, and log type is `server` or `agent`. The root logger generates an instance ID shared by its scoped children. `record_kind` defaults to `event`; callers override it with `measurement` for individual operations, `summary` for aggregate outcomes, or `snapshot` for point-in-time gauges.

- Use `log.Server(component)` for transport, startup, storage, broker, registry, and provider infrastructure. Use `log.Agent(component, requestID, canonicalUserID, gateway, model)` for request-scoped agent behavior: five string arguments, with no session argument. Request-scoped infrastructure also carries `request_id`; use `requestctx.LogFields` for available canonical/server-generated correlation.
- Events, components, and messages must be fixed developer-owned text. Use stable dotted event names and `config.F` with literal field keys. String keys require review in `stringLogFields`; the source must also allowlist or validate the actual values, not merely their keys. A syntactically valid label can still be private content. The output string filter is not a secret detector.
- Reviewed string labels are bounded to 256 UTF-8 bytes; invalid or oversized generic labels become `redacted`. Static messages are bounded to 1,024 bytes. Records are limited to 16 KiB; supported scalar arrays/slices retain at most 16 items. Safe numeric/bool metrics remain extensible. Named and unnamed scalar primitives are read without invoking custom string or JSON methods. Unknown string keys and private keys are omitted; unsupported objects, nonfinite numbers, or invalid/oversized payloads produce `logger.marshal_failed` with safe available correlation instead of serializing arbitrary objects.
- `config.ErrorField` emits only a fixed `error_code`, never raw error text. Typed SQLite numeric driver codes select storage classifications without logging SQLite messages. Do not restore `RawErrorField` or raw-error fields. `SafeErrorText` is for user responses only, not log content; unknown errors intentionally lose their details in logs.
- Safety applies equally at INFO and DEBUG. Never log prompts, responses, reasoning, tool arguments/results, URLs, external identities, phone numbers, email addresses, raw session/chat keys, image/base64 bytes, credentials, bootstrap codes, or duplicate fingerprints. Canonical user IDs and gateway names are allowed. Use reviewed operational labels, counts, sizes, durations, and reason codes instead of private content.
- IDs end in `_id`, counts in `_count`, durations in `_ms`, text sizes in `_chars`, and booleans begin with `is_`. Preserve existing units and historical keys when renaming Go counters. Keep numeric fields numeric in JSON. `status` is `ok`, `error`, `rejected`, `retry`, or `degraded`; `outcome` supplies additional meaning, such as `canceled`. Intentional cancellation is not itself an operational failure.

### Measurement Boundaries

`internal/llm/telemetry.go` owns one INFO `provider.gateway.chat.complete` or `provider.gateway.embed.complete` measurement per `Chat`/`Embed` invocation, including error and cancellation returns. These provider records are the authoritative observed token meter across foreground rounds, retries, formation, compaction, and indexing. Their `operation` values are `chat` and `embedding`, respectively. A call is not necessarily a submission: `is_submitted` is true only when a submission attempt is made, not during pre-submission validation/cancellation. Async status polls are not extra model submissions.

- `is_usage_reported` means at least one numeric usage field was reported, not that all usage is known. `is_usage_complete` requires success and available nonnegative prompt/total counts, plus completion counts for chat. `is_usage_invalid` marks observed negative usage. Only reported nonnegative token fields are logged; missing fields are not invented as zero or calculated from other fields.
- Cancellation/error can leave observed usage partial or unknown. A later invalid negative report does not erase a previously observed valid count. `internal/shared/requestctx/telemetry.go` collects only reported valid nonnegative counts, separating chat and embedding meters. A zero aggregate without its reported/complete flags does not prove zero remote work. Missing remote usage and collector loss prevent claims of complete billing or exactly-once ingestion.
- `duration_ms` is invocation wall time, including provider wait, network, and async polling, not raw decode time. `effective_output_tps` is reported completion tokens divided by that duration. `time_to_first_output_ms` is present only when streaming thinking/content is observable; it is not an async estimate or a first-tool-call timestamp.
- Provider per-call tokens, gateway `request_*` aggregates, and broker `execution_*` aggregates are different views of overlapping work. Never add all three levels together. Use provider records for observed usage totals, gateway summaries for addressed-request delivery, and execution summaries for actual processor completion.

`internal/gateway/runtime/executor.go` emits one INFO `gateway.request.received` after authentication and access checks admit a prompt or command, and one INFO `gateway.request.complete` per addressed runtime operation. Ignored messages do not become these request events. Pre-runtime transport/authentication rejections are separate gateway diagnostics, not admitted prompts. A runtime fallback or rejection can have a completion with `is_admitted=false` and no received record.

`request_kind` is `prompt`, `command`, or `fallback`. `prompt_type` is `text`, `text_image`, `image`, `unsupported`, or `empty`, classified from inbound content before generated reply/context enrichment. Completion includes `duration_ms`, `queue_wait_ms`, `agent_duration_ms`, `delivery_duration_ms`, separate `execution_status`, `delivery_status`, `persistence_status`, and request tool/model/embedding counters with usage flags. Delivery timing in `internal/gateway/runtime/telemetry.go` covers response sends, not indicators or durable post-delivery bookkeeping.

Stop can return immediately with broker result `ExecutionComplete=false`, before the current provider call reports usage. The gateway summary then has `is_execution_complete=false`, incomplete usage flags, and `persistence_status=unknown`; its counters can omit late work. `internal/broker/telemetry.go` emits `broker.request.execution.complete` on actual `Process` return, with eventual `execution_*` totals even after gateway completion. The execution snapshot is separate from the response, retaining counters when completion has an error and a nil response. Do not delay cancellation or add a goroutine just to wait for telemetry. `is_execution_complete` certifies processor return, not complete remote usage or delivery.

`internal/agent/agent.go` emits `agent.response.complete` for generation outcome and `agent.tool.complete` for each actual handler execution, with tool name/scope, operation correlation, duration, status/outcome, and bounded reason code. `agent.tool.blocked` records governance/authorization blocking separately; it is not an execution. Provider-specific tool diagnostics do not count as another tool execution. Request error rates come from terminal `gateway.request.complete` status, not the number of ERROR logs: `gateway.request.failed` is a separate diagnostic for the same failed operation.

### Background And Health

Workload values are `foreground`, `formation`, `compaction`, `indexing`, `maintenance`, and `system`. `memory_formation` is a `job_kind`, not a workload. Formation and compaction services use fresh attempt operation IDs, workload, job ID/kind, canonical ownership, and available persisted source-request/turn correlation. Workers do not fabricate gateway external identities; gateway and parent-operation information unavailable in persisted jobs is not reconstructed from private session keys. Foreground compaction overrides workload while preserving the request's usage collector and parent operation.

`internal/memory/formation/service.go` logs formation completion only after durable completion succeeds, including local saves, empty extraction, and artifact replay. Retry APIs return the successfully persisted state; `internal/compaction/service.go` uses stored submission/artifact state for retry/dead reporting rather than attempt-count guesses. Worker cancellation/preemption/refund outcomes are INFO; independent storage failures remain warnings.

`broker.health` is an INFO snapshot at broker lifecycle boundaries and every 30 seconds, with worker, queued, active, outstanding, capacity, oldest-queued-age, accepting, and background-active fields. `internal/memory/indexing/service.go` uses its existing 30-second indexing schedule for `memory.jobs.health`, `index.availability`, and `app.health`. A busy serialized indexing cycle can delay these snapshots. Snapshot reads share a scoped five-second timeout; failed reads emit diagnostic WARNs and omit unavailable data rather than claim zero backlogs.

Job gauges include queued/active/retry/dead/succeeded/skipped counts and `expired_lease_count`. `oldest_ready_age_ms` measures queued/retry time past `available_at`, not job creation age. Index availability is INFO when available and WARN when degraded. Failed rebuilds are WARN with no invented coverage/counts; successful rebuilds report validated live counts. Process gauges include goroutines, heap bytes, GC count, and `is_last_maintenance_known`. Last successful maintenance is process-local and unknown after restart until a sweep succeeds; `last_maintenance_age_ms` is omitted while unknown.

`internal/database/maintenance/service.go` reports every sweep at INFO or WARN, including unchanged sweeps: immediately at worker start, then hourly under the default policy. Successful ordinary sweeps emit INFO `maintenance.sweep.complete`; live-index degradation and failed phases emit WARN, and cancellation emits INFO. Counts include only committed mutations, retaining earlier committed phase counts if a later transaction rolls back. `rows_changed` counts committed operations, not distinct rows. Do not synthesize a successful zero-count summary after failure. `internal/database/migrations.go` logs migration application at INFO only after commit and foreign-key restoration, with applied count and prior/target release; unchanged opens are silent. `internal/startup/app.go` reports build metadata, initialization/cleanup boundaries, and `app.shutdown.complete` after acquired-resource cleanup, without implying gateway readiness or graceful listener shutdown.

### Contributor Rules

- Add safe INFO-level monitoring alongside every new operational path, even if nobody queries it today. Use bounded summaries and periodic gauges rather than payloads or hot-loop noise. WARN/ERROR diagnostics remain visible at the INFO threshold; DEBUG is supplemental only.
- Assign one owner to each terminal measurement/summary and emit it exactly once on all applicable success, failure, cancellation, rejection, empty-result, and replay paths. Distinguish attempts, retries, committed mutations, delivery outcomes, and final execution outcomes. Do not duplicate error diagnostics across layers or count diagnostics as additional operations.
- Propagate available request/operation/parent/job correlation and canonical ownership; never manufacture unavailable identity metadata. Record unknown/incomplete states explicitly. Preserve immediate cancellation, transaction/lease fencing, and committed-only counts rather than changing behavior to improve a metric.
- Test emission counts, levels, correlation, field types, missing/partial/negative usage, nil-response errors, late cancellation, rollback, and private-data canaries at INFO and DEBUG when changing those paths. `internal/config/logging_contract_test.go` is a limited AST guard for recognized logger/config-field conventions, not a general analyzer or proof of safety. Keep runtime logging boundary tests and source-value review.
- Keep Loki index labels low-cardinality: service, level, log type, component, event, optionally gateway. Request/operation/instance/user/job IDs, tool names, model names, and numeric measurements remain JSON fields, not high-cardinality ingestion labels. Query-time extraction/grouping does not require promoting them to index labels. Preserve the schema and document changed event semantics here.

### LogQL Recipes

These examples assume the collector promotes `service` to a Loki label, so `{service="oswald-ai"}` selects stderr JSON records. Adjust that selector to the environment. Other fields are extracted at query time; use canonical `user_id`, never external identity. Filter `__error__=""` after JSON parsing and again after numeric `unwrap` to exclude parse/conversion errors. Windows and grouping are examples, not dashboards or ingestion guarantees.

1. Admitted prompt count over one hour, by gateway, canonical user, and inbound prompt type (excludes commands):

```logql
sum by (gateway, user_id, prompt_type) (count_over_time({service="oswald-ai"} | json | __error__="" | event="gateway.request.received" | request_kind="prompt" | is_admitted="true" [1h]))
```

2. Admitted addressed-request p95 end-to-end latency in milliseconds:

```logql
quantile_over_time(0.95, {service="oswald-ai"} | json | __error__="" | event="gateway.request.complete" | is_admitted="true" | unwrap duration_ms | __error__="" [5m]) by (gateway)
```

3. Observed chat total tokens, including reported usage from failed/canceled calls; calls reporting only prompt/completion counts cannot contribute a total:

```logql
sum by (user_id, gateway, model, workload) (sum_over_time({service="oswald-ai"} | json | __error__="" | event="provider.gateway.chat.complete" | operation="chat" | is_usage_reported="true" | total_tokens!="" | unwrap total_tokens | __error__="" [1h]))
```

4. Mean per-call effective output TPS for chat calls with complete usage (not fleet decode throughput):

```logql
avg_over_time({service="oswald-ai"} | json | __error__="" | event="provider.gateway.chat.complete" | operation="chat" | is_usage_complete="true" | effective_output_tps!="" | unwrap effective_output_tps | __error__="" [5m]) by (model)
```

5. Actual tool executions by name, excluding blocked calls:

```logql
sum by (tool_name) (count_over_time({service="oswald-ai"} | json | __error__="" | event="agent.tool.complete" [1h]))
```

6. Tool execution failure fraction by name (canceled/degraded outcomes are not `status=error`):

```logql
sum by (tool_name) (rate({service="oswald-ai"} | json | __error__="" | event="agent.tool.complete" | status="error" [5m]))
/
sum by (tool_name) (rate({service="oswald-ai"} | json | __error__="" | event="agent.tool.complete" [5m]))
```

7. Admitted request failure fraction by gateway, based on terminal summaries rather than ERROR-line counts:

```logql
sum by (gateway) (rate({service="oswald-ai"} | json | __error__="" | event="gateway.request.complete" | is_admitted="true" | status="error" [5m]))
/
sum by (gateway) (rate({service="oswald-ai"} | json | __error__="" | event="gateway.request.complete" | is_admitted="true" [5m]))
```

8. Latest queued-job gauge per kind and instance, not the sum of historical snapshots:

```logql
last_over_time({service="oswald-ai"} | json event="event", job_kind="job_kind", instance_id="instance_id", queued_count="queued_count" | __error__="" | event="memory.jobs.health" | unwrap queued_count | __error__="" [5m]) by (job_kind, instance_id)
```

An absent rate series is not necessarily an explicit zero; a missing snapshot may be delayed or failed. The gauge can be stale within its lookback window. Do not sum database-wide gauges from multiple instances sharing a database. For investigation, filter parsed `request_id`/`operation_id` or `job_id` on log queries rather than adding index labels. None of these queries can recover unreported provider usage, lost log records, or deduplicate collector re-ingestion automatically.

## Extension Checklist

### Tools

1. Add the stable builtin name in `internal/tools/names/names.go` and its schema in `data/tools/`; extend the exact schema/name contract test. Private model tools are not builtin catalog entries.
2. Implement the handler under its builtin domain; put shared persistence in the owning domain package. Require authenticated principals for tenant-sensitive work and derive ownership from context, not model arguments.
3. Register explicit governance, argument normalization, and durable-history policy in `internal/tools/builtin/register.go`. Update stream-status rendering if needed without exposing sensitive arguments/results.
4. Cover enablement, validation, permissions, duplicate/failure behavior, cancellation, bounded output, and advertised schema. Update the inventory here and configuration examples only if configuration changes.

### Commands And Gateways

1. Commands belong under `internal/commands` adapters and register through builtin composition. `AdminOnly` is metadata: install middleware or enforce authorization explicitly. Sensitive fence resolvers must authorize themselves because resolution precedes middleware execution.
2. Use broker fences for canonical-user mutations and test them through delivery/invalidation. Out-of-band handlers cannot implement `FenceTargetResolver`; `HandlerFunc` implements that interface, so `/stop`-style commands need a dedicated handler type.
3. New gateways implement `gateway.Service`, normalize inbound requests, resolve authenticated principals, and provide `runtime.Responder`. Add assurance validity tests and wire construction only in `gateway/bootstrap.go`.
4. Verify group routing, reply handling, cancellation, delivery acknowledgement, attachments, and streaming. Attachment support is not yet a generic gateway capability contract; update tool visibility deliberately for a new text-only gateway.

### Persistence And Policy

1. Add exactly one new semantically named SQL migration for a schema release. Never modify released SQL or add a pre-v4 importer implicitly.
2. Preserve tenant/source/delivery/lease checks, foreign keys, JSON references, non-reusable IDs/high-water, and atomic outbox writes. Add fresh, supported-prefix, checksum-rejection, rollback, foreign-key, concurrent-open, and reopen coverage.
3. Version persisted artifact changes explicitly and retain decoders needed by existing v4 data. Do not rename JSON/schema/tool/log contracts as a side effect of Go cleanup.
4. Keep profile compilation deterministic, make current and retained legacy policy distinctions explicit, and test merge/reset/deletion/replay paths with any memory change.
5. Soul changes are operator filesystem edits to `data/memory/soul/soul.md`, applied on the next request; no model tool may mutate that policy.
