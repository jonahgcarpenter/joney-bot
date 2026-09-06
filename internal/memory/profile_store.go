package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SessionProfile is the immutable tenant profile bound to one session generation.
type SessionProfile struct {
	VersionID       int64
	Version         int
	LatestVersion   int
	LatestFactCount int
	LatestBytes     int
	Generation      int
	SpeakerIntro    string
	Content         string
	FactCount       int
	Bytes           int
	IsNewVersion    bool
	IsNewSession    bool
	sourceDigest    string
}

// ResetSessionContext clears and refreshes one session using the standard TTL.
func (s *Store) ResetSessionContext(ctx context.Context, userID, sessionID string) error {
	_, err := s.ResetSession(ctx, userID, sessionID, s.sessionTTL(24*time.Hour))
	return err
}

// ResolveSessionProfile returns the frozen profile for a session, creating a
// new profile version or session generation when required.
func (s *Store) ResolveSessionProfile(ctx context.Context, userID, sessionID string, ttl time.Duration) (SessionProfile, error) {
	if err := s.ensureAccountUser(userID); err != nil {
		return SessionProfile{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return SessionProfile{}, fmt.Errorf("tenant profile: session id is required")
	}
	ttl = s.sessionTTL(ttl)
	now := time.Now().UTC()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return SessionProfile{}, fmt.Errorf("begin tenant profile resolution: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	current, sourceIDs, err := refreshProfileTx(ctx, tx, userID, now)
	if err != nil {
		return SessionProfile{}, err
	}
	var generation, isActive int
	var expiresRaw string
	err = tx.QueryRowContext(ctx, `SELECT generation, is_active, expires_at FROM sessions WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID).Scan(&generation, &isActive, &expiresRaw)
	if err != nil && err != sql.ErrNoRows {
		return SessionProfile{}, fmt.Errorf("read tenant session profile: %w", err)
	}
	if err == nil && isActive != 0 {
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresRaw)
		if parseErr == nil && expiresAt.After(now) {
			profile, loadErr := loadSessionProfileTx(ctx, tx, userID, sessionID)
			if loadErr != nil {
				return SessionProfile{}, loadErr
			}
			if _, updateErr := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE canonical_user_id = ? AND session_id = ?`, formatTime(now), formatTime(now.Add(ttl)), userID, sessionID); updateErr != nil {
				return SessionProfile{}, fmt.Errorf("refresh tenant session expiry: %w", updateErr)
			}
			profile.Generation = generation
			profile.IsNewVersion = current.IsNewVersion
			profile.LatestVersion = current.Version
			profile.LatestFactCount = current.FactCount
			profile.LatestBytes = current.Bytes
			if err := tx.Commit(); err != nil {
				return SessionProfile{}, fmt.Errorf("commit tenant profile resolution: %w", err)
			}
			return profile, nil
		}
	}
	isNewSession := true
	if err == nil {
		generation++
	} else {
		generation = 1
		if scanErr := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(session_generation), 0) + 1 FROM session_turns WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID).Scan(&generation); scanErr != nil {
			return SessionProfile{}, fmt.Errorf("resolve tenant session generation: %w", scanErr)
		}
	}
	if err := bindSessionProfileTx(ctx, tx, userID, sessionID, generation, current, sourceIDs, now, ttl); err != nil {
		return SessionProfile{}, err
	}
	current.Generation = generation
	current.LatestVersion = current.Version
	current.LatestFactCount = current.FactCount
	current.LatestBytes = current.Bytes
	current.IsNewSession = isNewSession
	if err := tx.Commit(); err != nil {
		return SessionProfile{}, fmt.Errorf("commit tenant profile binding: %w", err)
	}
	return current, nil
}

// ResetSession clears one tenant's conversation history and binds the latest profile.
func (s *Store) ResetSession(ctx context.Context, userID, sessionID string, ttl time.Duration) (SessionProfile, error) {
	if err := s.ensureAccountUser(userID); err != nil {
		return SessionProfile{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return SessionProfile{}, fmt.Errorf("tenant profile: session id is required")
	}
	ttl = s.sessionTTL(ttl)
	now := time.Now().UTC()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return SessionProfile{}, fmt.Errorf("begin tenant session reset: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	current, sourceIDs, err := refreshProfileTx(ctx, tx, userID, now)
	if err != nil {
		return SessionProfile{}, err
	}
	var generation int
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(generation), 0) + 1 FROM (
	SELECT generation FROM sessions WHERE canonical_user_id = ? AND session_id = ?
	UNION ALL SELECT session_generation FROM session_turns WHERE canonical_user_id = ? AND session_id = ?
)`, userID, sessionID, userID, sessionID).Scan(&generation); err != nil {
		return SessionProfile{}, fmt.Errorf("resolve reset session generation: %w", err)
	}
	if generation <= 0 {
		generation = 1
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM durable_jobs WHERE job_kind = 'memory_formation' AND canonical_user_id = ? AND source_session_id = ?`, userID, sessionID); err != nil {
		return SessionProfile{}, fmt.Errorf("clear reset session formation jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ?`, userID, sessionID); err != nil {
		return SessionProfile{}, fmt.Errorf("clear reset session compaction jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_summaries WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID); err != nil {
		return SessionProfile{}, fmt.Errorf("clear reset session summaries: %w", err)
	}
	turnIDs, err := memoryIDsTx(tx, `SELECT id FROM session_turns WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID)
	if err != nil {
		return SessionProfile{}, fmt.Errorf("enumerate reset session turns: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_turns WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID); err != nil {
		return SessionProfile{}, fmt.Errorf("clear reset session turns: %w", err)
	}
	for _, id := range turnIDs {
		if err := enqueueDerivedChangeTx(ctx, tx, userID, "session_turn", id, "delete", "reset:"+formatTime(now)); err != nil {
			return SessionProfile{}, err
		}
	}
	if err := bindSessionProfileTx(ctx, tx, userID, sessionID, generation, current, sourceIDs, now, ttl); err != nil {
		return SessionProfile{}, err
	}
	current.Generation = generation
	current.LatestVersion = current.Version
	current.LatestFactCount = current.FactCount
	current.LatestBytes = current.Bytes
	current.IsNewSession = true
	if err := tx.Commit(); err != nil {
		return SessionProfile{}, fmt.Errorf("commit tenant session reset: %w", err)
	}
	s.signalDerivedIndex()
	return current, nil
}

func bindSessionProfileTx(ctx context.Context, tx *sql.Tx, userID, sessionID string, generation int, profile SessionProfile, sourceIDs []int64, now time.Time, ttl time.Duration) error {
	encoded, err := json.Marshal(sourceIDs)
	if err != nil {
		return fmt.Errorf("encode tenant profile sources: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sessions (canonical_user_id, session_id, generation, is_active, last_seen_at, expires_at,
	profile_version, profile_version_high_water, renderer_version, source_digest, speaker_intro, rendered_content,
	source_memory_ids)
VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id, session_id) DO UPDATE SET
	generation = excluded.generation, is_active = 1,
	last_seen_at = excluded.last_seen_at, expires_at = excluded.expires_at,
	profile_version = excluded.profile_version,
	profile_version_high_water = MAX(sessions.profile_version_high_water, excluded.profile_version_high_water),
	renderer_version = excluded.renderer_version, source_digest = excluded.source_digest,
	speaker_intro = excluded.speaker_intro, rendered_content = excluded.rendered_content,
	source_memory_ids = excluded.source_memory_ids
`, userID, sessionID, generation, formatTime(now), formatTime(now.Add(ttl)),
		profile.Version, profile.Version, ProfileRendererVersion, profile.sourceDigest, profile.SpeakerIntro,
		profile.Content, string(encoded)); err != nil {
		return fmt.Errorf("bind tenant session profile: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET profile_version_high_water = MAX(profile_version_high_water, ?) WHERE canonical_user_id = ?`, profile.Version, userID); err != nil {
		return fmt.Errorf("advance tenant profile high-water: %w", err)
	}
	return nil
}

func refreshProfileTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (SessionProfile, []int64, error) {
	var intro string
	err := tx.QueryRowContext(ctx, `SELECT speaker_intro FROM account_users WHERE canonical_user_id = ?`, userID).Scan(&intro)
	if err != nil && err != sql.ErrNoRows {
		return SessionProfile{}, nil, fmt.Errorf("read tenant profile intro: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id, category, statement, scope, status, 1, confidence, importance, expires_at, provenance_type,
	CASE provenance_type WHEN 'user_statement' THEN 'user_direct' WHEN 'model_inference' THEN 'model' ELSE 'unknown' END
FROM memory_entries WHERE canonical_user_id = ?`, userID)
	if err != nil {
		return SessionProfile{}, nil, fmt.Errorf("read tenant profile candidates: %w", err)
	}
	var candidates []ProfileCandidate
	for rows.Next() {
		var candidate ProfileCandidate
		var expires sql.NullString
		if err := rows.Scan(&candidate.MemoryID, &candidate.Category, &candidate.Statement, &candidate.Scope, &candidate.Status, &candidate.Approved, &candidate.Confidence, &candidate.Importance, &expires, &candidate.FormationProvenance, &candidate.SourceAuthority); err != nil {
			rows.Close()
			return SessionProfile{}, nil, fmt.Errorf("scan tenant profile candidate: %w", err)
		}
		if expires.Valid {
			candidate.ExpiresAt = parseTime(expires.String)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Close(); err != nil {
		return SessionProfile{}, nil, fmt.Errorf("close tenant profile candidates: %w", err)
	}
	compiled := CompileProfile(intro, candidates, now)
	sourceIDs := make([]int64, 0, len(compiled.SelectedFacts))
	for _, fact := range compiled.SelectedFacts {
		sourceIDs = append(sourceIDs, fact.MemoryID)
	}
	profile := SessionProfile{SpeakerIntro: normalizeProfileText(intro), Content: compiled.Content, FactCount: compiled.SelectedCount, Bytes: compiled.Bytes, sourceDigest: compiled.SourceDigest}
	var latestVersion int
	var latestDigest, latestRenderer string
	err = tx.QueryRowContext(ctx, `SELECT profile_version, source_digest, renderer_version FROM sessions WHERE canonical_user_id = ? ORDER BY profile_version DESC LIMIT 1`, userID).Scan(&latestVersion, &latestDigest, &latestRenderer)
	if err != nil && err != sql.ErrNoRows {
		return SessionProfile{}, nil, fmt.Errorf("read current tenant profile version: %w", err)
	}
	if err == nil && latestDigest == compiled.SourceDigest && latestRenderer == ProfileRendererVersion {
		profile.Version = latestVersion
		profile.VersionID = int64(latestVersion)
		return profile, sourceIDs, nil
	}
	var highWater int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(profile_version_high_water), 0) FROM sessions WHERE canonical_user_id = ?`, userID).Scan(&highWater); err != nil {
		return SessionProfile{}, nil, fmt.Errorf("allocate tenant profile version: %w", err)
	}
	profile.Version = highWater + 1
	profile.VersionID = int64(profile.Version)
	profile.IsNewVersion = true
	return profile, sourceIDs, nil
}

func loadSessionProfileTx(ctx context.Context, tx *sql.Tx, userID, sessionID string) (SessionProfile, error) {
	var profile SessionProfile
	err := tx.QueryRowContext(ctx, `SELECT profile_version, profile_version, speaker_intro, rendered_content, json_array_length(source_memory_ids) FROM sessions WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID).Scan(&profile.VersionID, &profile.Version, &profile.SpeakerIntro, &profile.Content, &profile.FactCount)
	if err != nil {
		return SessionProfile{}, fmt.Errorf("load tenant session profile: %w", err)
	}
	profile.Bytes = len(profile.Content)
	return profile, nil
}

func rebindProfileCopiesTx(ctx context.Context, tx *sql.Tx, userID string, memoryID int64, now time.Time) error {
	profile, sourceIDs, err := refreshProfileTx(ctx, tx, userID, now)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(sourceIDs)
	if err != nil {
		return fmt.Errorf("encode rebound profile sources: %w", err)
	}
	condition := `EXISTS (SELECT 1 FROM json_each(sessions.source_memory_ids) source JOIN memory_entries memory ON memory.id = CAST(source.value AS INTEGER) WHERE memory.canonical_user_id = ? AND memory.status = 'expired')`
	args := []any{profile.Version, profile.Version, ProfileRendererVersion, profile.sourceDigest, profile.SpeakerIntro, profile.Content, string(encoded), userID, userID}
	if memoryID > 0 {
		condition = `EXISTS (SELECT 1 FROM json_each(sessions.source_memory_ids) source WHERE CAST(source.value AS INTEGER) = ?)`
		args = []any{profile.Version, profile.Version, ProfileRendererVersion, profile.sourceDigest, profile.SpeakerIntro, profile.Content, string(encoded), userID, memoryID}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET
	profile_version = ?, profile_version_high_water = MAX(profile_version_high_water, ?), renderer_version = ?,
	source_digest = ?, speaker_intro = ?, rendered_content = ?,
	source_memory_ids = ?
WHERE canonical_user_id = ? AND `+condition, args...); err != nil {
		return fmt.Errorf("rebind tenant profile snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET profile_version_high_water = MAX(profile_version_high_water, ?) WHERE canonical_user_id = ?`, profile.Version, userID); err != nil {
		return fmt.Errorf("advance rebound profile high-water: %w", err)
	}
	return nil
}
