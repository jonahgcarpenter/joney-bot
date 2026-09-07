-- Replace just the checked column, preserving bytes and the turn ID high-water.
ALTER TABLE session_turns ADD COLUMN foreground_memory_next TEXT NOT NULL DEFAULT '{"version":3,"candidates":[]}' CHECK (
 length(CAST(foreground_memory_next AS BLOB)) <= 32768 AND json_valid(foreground_memory_next)
 AND json_type(foreground_memory_next) = 'object'
 AND json_extract(foreground_memory_next,'$.version') IN (2,3)
 AND json_type(foreground_memory_next,'$.candidates') = 'array'
 AND json_array_length(foreground_memory_next,'$.candidates') <= 5
);
UPDATE session_turns SET foreground_memory_next = foreground_memory;
DROP TRIGGER session_turns_foreground_memory_update;
ALTER TABLE session_turns DROP COLUMN foreground_memory;
ALTER TABLE session_turns RENAME COLUMN foreground_memory_next TO foreground_memory;
CREATE TRIGGER session_turns_foreground_memory_update
BEFORE UPDATE OF foreground_memory ON session_turns
WHEN NEW.foreground_memory IS NOT OLD.foreground_memory
BEGIN
 SELECT RAISE(ABORT, 'immutable session turn foreground memory');
END;

-- Admitted observations deliberately do not reference transient session turns.
ALTER TABLE memory_entries ADD COLUMN revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0);
ALTER TABLE memory_entries ADD COLUMN assessed_source_turn_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE memory_entries ADD COLUMN assessment_context TEXT NOT NULL DEFAULT '' CHECK(length(CAST(assessment_context AS BLOB))<=4000);
ALTER TABLE memory_entries ADD COLUMN retired_at TEXT;
ALTER TABLE memory_entries ADD COLUMN retirement_reason TEXT NOT NULL DEFAULT '' CHECK(retirement_reason IN ('','retired','replaced','low_confidence'));
CREATE TRIGGER memory_entries_assessment_revision
AFTER UPDATE OF statement, confidence, provenance_type, status, claim_slot, claim_value, assessment_context, retired_at, retirement_reason ON memory_entries
WHEN NEW.statement IS NOT OLD.statement OR NEW.confidence IS NOT OLD.confidence
 OR NEW.provenance_type IS NOT OLD.provenance_type OR NEW.status IS NOT OLD.status
 OR NEW.claim_slot IS NOT OLD.claim_slot OR NEW.claim_value IS NOT OLD.claim_value
 OR NEW.assessment_context IS NOT OLD.assessment_context OR NEW.retired_at IS NOT OLD.retired_at OR NEW.retirement_reason IS NOT OLD.retirement_reason
BEGIN
 UPDATE memory_entries SET revision = OLD.revision + 1 WHERE id = NEW.id;
END;

CREATE TABLE memory_observations (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 canonical_user_id TEXT NOT NULL REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
 source_turn_id INTEGER NOT NULL CHECK(source_turn_id>0),
 statement TEXT NOT NULL, evidence TEXT NOT NULL, context TEXT NOT NULL,
 provenance_type TEXT NOT NULL, claim_slot TEXT NOT NULL, claim_value TEXT NOT NULL,
 observed_at TEXT NOT NULL CHECK(julianday(observed_at) IS NOT NULL), expires_at TEXT NOT NULL CHECK(julianday(expires_at)>julianday(observed_at) AND julianday(expires_at)<=julianday(observed_at)+30),
 intent TEXT NOT NULL DEFAULT 'automatic' CHECK(intent IN ('automatic','remember')),
 UNIQUE(canonical_user_id, source_turn_id, claim_slot, claim_value, evidence)
);
CREATE INDEX memory_observations_owner ON memory_observations(canonical_user_id, expires_at);
CREATE TABLE memory_observation_receipts (
 canonical_user_id TEXT NOT NULL REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
 source_turn_id INTEGER NOT NULL CHECK(source_turn_id>0),
 digest TEXT NOT NULL CHECK(length(digest)=64),
 PRIMARY KEY(canonical_user_id,source_turn_id,digest)
);
CREATE TRIGGER memory_observation_receipt_insert
BEFORE INSERT ON memory_observation_receipts
WHEN (SELECT COUNT(*) FROM memory_observation_receipts WHERE canonical_user_id=NEW.canonical_user_id AND source_turn_id=NEW.source_turn_id)>=5
 OR NOT EXISTS(SELECT 1 FROM session_turns WHERE id=NEW.source_turn_id AND canonical_user_id=NEW.canonical_user_id AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL)
BEGIN SELECT RAISE(ABORT,'invalid observation admission receipt'); END;

CREATE TRIGGER memory_observation_source_insert
BEFORE INSERT ON memory_observations
WHEN NOT EXISTS(SELECT 1 FROM session_turns t WHERE t.id=NEW.source_turn_id AND t.canonical_user_id=NEW.canonical_user_id AND t.delivered_at IS NOT NULL AND t.delivery_failed_at IS NULL AND julianday(t.created_at)=julianday(NEW.observed_at))
 OR (SELECT COUNT(*) FROM memory_observations WHERE canonical_user_id=NEW.canonical_user_id)>=100
 OR (SELECT COUNT(*) FROM memory_observations WHERE canonical_user_id=NEW.canonical_user_id AND source_turn_id=NEW.source_turn_id)>=5
 OR COALESCE((SELECT SUM(length(CAST(statement||evidence||context||provenance_type||claim_slot||claim_value AS BLOB))) FROM memory_observations WHERE canonical_user_id=NEW.canonical_user_id),0)+length(CAST(NEW.statement||NEW.evidence||NEW.context||NEW.provenance_type||NEW.claim_slot||NEW.claim_value AS BLOB))>131072
BEGIN SELECT RAISE(ABORT,'invalid observation source or capacity'); END;
CREATE TABLE memory_suppressions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 canonical_user_id TEXT NOT NULL REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
 statement TEXT NOT NULL, claim_slot TEXT NOT NULL, claim_value TEXT NOT NULL,
 is_active INTEGER NOT NULL DEFAULT 1 CHECK(is_active IN (0,1)),
 through_turn_id INTEGER NOT NULL, created_at TEXT NOT NULL,
 retained_source_turn_id INTEGER NOT NULL DEFAULT 0 CHECK(retained_source_turn_id>=0),
 UNIQUE(canonical_user_id, claim_slot, claim_value)
);
CREATE TABLE memory_assessment_inputs (
 job_id INTEGER PRIMARY KEY REFERENCES durable_jobs(id) ON DELETE CASCADE,
 canonical_user_id TEXT NOT NULL REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
 payload TEXT NOT NULL CHECK(length(CAST(payload AS BLOB))<=1048576 AND json_valid(payload) AND json_type(payload)='object' AND json_extract(payload,'$.Version') IS 1)
);
CREATE TABLE memory_assessment_receipts (
 canonical_user_id TEXT NOT NULL REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
 source_turn_id INTEGER NOT NULL, purpose TEXT NOT NULL,
 digest TEXT NOT NULL CHECK(length(digest)=64),
 PRIMARY KEY(canonical_user_id, source_turn_id, purpose)
);
CREATE TABLE memory_observation_evidence (
 memory_id INTEGER NOT NULL REFERENCES memory_entries(id) ON DELETE CASCADE,
 observation_id INTEGER NOT NULL REFERENCES memory_observations(id) ON DELETE CASCADE,
 PRIMARY KEY(memory_id, observation_id)
);

CREATE TRIGGER memory_observation_evidence_insert
BEFORE INSERT ON memory_observation_evidence
WHEN NOT EXISTS (SELECT 1 FROM memory_entries m JOIN memory_observations o
 ON o.canonical_user_id=m.canonical_user_id WHERE m.id=NEW.memory_id AND o.id=NEW.observation_id)
 OR ((SELECT COUNT(*) FROM memory_observation_evidence WHERE memory_id=NEW.memory_id)>=5
 AND NOT EXISTS(SELECT 1 FROM memory_observation_evidence WHERE memory_id=NEW.memory_id AND observation_id=NEW.observation_id))
BEGIN
 SELECT RAISE(ABORT, 'invalid observation evidence relationship');
END;

CREATE TRIGGER durable_jobs_assessment_context_insert
BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind='memory_formation' AND NEW.extractor_version='assessment-v1' AND (
 NEW.formation_purpose!='background_pattern' OR NOT json_valid(NEW.artifact_payload)
 OR json_type(NEW.artifact_payload) IS NOT 'object'
 OR json_extract(NEW.artifact_payload,'$.version') IS NOT 1
 OR json_type(NEW.artifact_payload,'$.turn_ids') IS NOT 'array'
 OR json_array_length(NEW.artifact_payload,'$.turn_ids') NOT BETWEEN 1 AND 8
 OR (SELECT COUNT(*) FROM json_each(NEW.artifact_payload))!=2
 OR EXISTS(SELECT 1 FROM json_each(NEW.artifact_payload,'$.turn_ids') WHERE type!='integer' OR value<=0)
 OR (SELECT COUNT(DISTINCT value) FROM json_each(NEW.artifact_payload,'$.turn_ids'))!=json_array_length(NEW.artifact_payload,'$.turn_ids')
 OR json_extract(NEW.artifact_payload,'$.turn_ids[#-1]') IS NOT NEW.source_turn_id
 OR length(CAST(NEW.artifact_payload AS BLOB))>1024
 OR EXISTS(SELECT 1 FROM json_each(NEW.artifact_payload,'$.turn_ids') member WHERE NOT EXISTS(SELECT 1 FROM session_turns t WHERE t.id=member.value AND t.canonical_user_id=NEW.canonical_user_id AND t.session_id=NEW.source_session_id AND t.session_generation=NEW.source_session_generation AND t.delivered_at IS NOT NULL AND t.delivery_failed_at IS NULL))
)
BEGIN
 SELECT RAISE(ABORT, 'invalid durable assessment context');
END;

CREATE TRIGGER memory_assessment_input_source_insert
BEFORE INSERT ON memory_assessment_inputs
WHEN NOT EXISTS(SELECT 1 FROM durable_jobs j WHERE j.id=NEW.job_id AND j.job_kind='memory_formation'
 AND j.canonical_user_id=NEW.canonical_user_id
 AND json_extract(NEW.payload,'$.Anchor.ID') IS j.source_turn_id
 AND json_extract(NEW.payload,'$.Anchor.UserID') IS j.canonical_user_id
 AND json_extract(NEW.payload,'$.Anchor.SessionID') IS j.source_session_id
 AND json_extract(NEW.payload,'$.Anchor.Generation') IS j.source_session_generation)
BEGIN SELECT RAISE(ABORT,'invalid frozen assessment source identity'); END;
