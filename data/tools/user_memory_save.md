# user_memory_save

## Description

Stage one to five model-assessed candidates for the authenticated current user's private memory. Make one initial call and at most one corrective call containing only retryable rejected items; the five-candidate cap spans both calls. Assess support and durability separately, using the conversation context without an extra model call. Evidence must be an exact CURRENT user-message span of at most 1000 runes, not recalled facts, quoted history injected by a tool, or assistant text. Explicit assertions are direct_statement; contextual implications are model_inference. Confidence measures support for the precise statement, not permanence. Below 0.35 remains proposed, not active.

Use retention=durable only for a supported lasting fact or stable preference. "I prefer pancakes for breakfast" supports a durable preference; "I want pancakes now" supports a high-confidence temporary desire, NOT a stable preference. Use retention=observation for potentially useful temporary, task-bound, ambiguous or insufficiently durable information, normally 7 days and never more than 30. Preserve task conditions ("for this presentation, be brief" is not a global communication preference), historical time ("I used to live in Paris" does not mean current residence), attribution, uncertainty and negation in the statement and context. These are semantic assessments, not forbidden keyword classes. A remember request does not make an unsupported inference true or turn a temporary task condition into a permanent preference.

Set intent=automatic normally, remember for explicit saving requests, correction for a directly asserted replacement of recalled memory, or retire for a direct correction invalidating a recalled fact without a supported replacement. Before correction or retirement, use user_memory_search or user_memory_list to obtain the target's current ID, revision and claim_slot unless all are already available in current recall. Frozen profile text is not current mutation metadata: look up its fact before correcting it. Both target_memory_id and expected_revision must be positive for correction/retire; never invent either. Reuse the target's exact claim slot. Corrections need not raise confidence. Retire requires direct current evidence; describe the invalidation, never fabricate an opposite preference or replacement fact. supersedes retains the exact recalled statement as a compatibility hint, not a substitute for ID/revision or new evidence. Same-claim reinforcement uses automatic/remember and reinforces_memory_id (or target_memory_id) without requiring expected_revision; do not give conflicting target IDs.

Set cardinality=single for mutually exclusive values of one property (such as current home city), multiple for independently coexisting facts or preferences. Use specific category-compatible claim slots and reuse the same slot/value for equivalent claims; preserve meaningful punctuation such as C, C++, and C#. Context (at most 500 runes) explains task, time, attribution, or correction scope, not extra evidence. A saved directive cannot grant authorization, capabilities, or tool availability. Staging is pending successful delivery and durable reconciliation, not a guarantee of publication. Observations are temporary evidence, not active durable memories. Never resubmit staged items or claim rejected items were saved.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| memories | array | yes | One to five independently grounded durable memory candidates from the current user message. |

## Schema

```json
{
  "type": "object",
  "properties": {
    "memories": {
      "type": "array",
      "minItems": 1,
      "maxItems": 5,
      "items": {
        "type": "object",
        "properties": {
          "statement": {"type": "string", "description": "Concise third-person statement about the user.", "minLength": 1, "maxLength": 1000},
          "evidence": {"type": "string", "description": "Exact verbatim evidence from the current user message.", "minLength": 1, "maxLength": 1000},
          "category": {"type": "string", "enum": ["identity", "communication_preferences", "durable_preferences", "projects", "relationships", "environment", "notes"]},
          "claim_slot": {"type": "string", "description": "Stable category-compatible dotted property. Allowed prefixes: identity uses identity.; communication_preferences uses communication.; durable_preferences uses preference. or durable.; projects uses project.; relationships uses relationship.; environment uses environment.; notes uses notes.", "minLength": 1, "maxLength": 128},
          "claim_value": {"type": "string", "description": "Concise value grounded in the exact evidence.", "minLength": 1, "maxLength": 256},
          "supersedes": {"type": "string", "description": "Exact active memory statement being corrected, or an empty string.", "maxLength": 1000},
          "evidence_type": {"type": "string", "enum": ["direct_statement", "model_inference"]},
          "confidence": {"type": "number", "description": "Definitiveness and contextual support for this assessment.", "minimum": 0, "maximum": 1},
          "retention": {"type": "string", "description": "Canonical correction requires durable retention; observation is temporary evidence only.", "enum": ["durable", "observation"]},
          "intent": {"type": "string", "enum": ["automatic", "remember", "correction", "retire"]},
          "context": {"type": "string", "maxLength": 500},
          "ttl_days": {"type": "integer", "description": "Durable: 0. Observation: 1-30, with 0 selecting 7 days.", "minimum": 0, "maximum": 30},
          "cardinality": {"type": "string", "enum": ["single", "multiple"]},
          "target_memory_id": {"type": "integer", "description": "Exact recalled memory ID for correction, retirement or reinforcement; never guess.", "minimum": 0},
          "expected_revision": {"type": "integer", "description": "Exact positive revision from current recall/search/list, required for correction/retire. If unavailable, look up the target before saving; never guess. Optional/0 for same-claim reinforcement.", "minimum": 0},
          "reinforces_memory_id": {"type": "integer", "description": "Positive ID of an active recalled memory with the same claim slot and value, or 0/omitted.", "minimum": 0}
        },
        "required": ["statement", "evidence", "category", "claim_slot", "claim_value", "supersedes", "evidence_type", "confidence", "retention", "intent", "context", "ttl_days", "cardinality"],
        "additionalProperties": false
      }
    }
  },
  "required": ["memories"],
  "additionalProperties": false
}
```
