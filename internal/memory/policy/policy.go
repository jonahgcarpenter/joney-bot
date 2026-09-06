package policy

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxSourceRunes          = 16_000
	maxStatementRunes       = 1_000
	maxEvidenceRunes        = 1_000
	maxClaimSlotRunes       = 128
	maxClaimValueRunes      = 256
	minimumActiveConfidence = 0.35
	explicitConfidenceFloor = 0.90
	defaultTaskTTL          = 24 * time.Hour
	minTaskTTL              = time.Hour
	maxTaskTTL              = 30 * 24 * time.Hour
)

var errInvalidCandidate = errors.New("invalid memory candidate")

var credentialTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\b(AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{10,}|sk-[A-Za-z0-9_-]{20,})\b`),
}

var namedCredentialMaterialPattern = regexp.MustCompile(`(?i)\b(api[ _-]?key|access[ _-]?token|auth[ _-]?token|refresh[ _-]?token|client[ _-]?secret|webhook[ _-]?secret|password|passwd|private[ _-]?key|encryption[ _-]?key|signing[ _-]?key|secret)(?:\s+for\s+[a-z0-9._ -]{1,64})?\s*(?:is|[:=])\s*([^\s,;]+)`)

// Evaluate validates, normalizes, and deterministically classifies a candidate.
func Evaluate(in CandidateInput) (CandidateOutput, error) {
	if err := validateCandidate(in); err != nil {
		return CandidateOutput{}, err
	}

	out := CandidateOutput{
		Statement:       normalizeText(in.Statement),
		Evidence:        normalizeText(in.Evidence),
		Provenance:      in.Provenance,
		SourceAuthority: authorityFor(in.Provenance),
		Sensitivity:     maxSensitivity(in.Sensitivity, ClassifySensitivity(in.Statement, in.Category)),
		Mode:            in.Mode,
		Scope:           in.Scope,
		Category:        in.Category,
		Context:         in.Context,
		Confidence:      in.Confidence,
		Importance:      in.Importance,
		TTL:             in.TTL,
		Approval:        ApprovalProposed,
		Decision:        DecisionProposed,
		Reason:          "candidate requires review",
	}
	out.ClaimSlot, out.ClaimValue = NormalizeClaimIdentity(in.Category, in.ClaimSlot, in.ClaimValue, out.Statement)
	if !claimSlotCompatible(in.Category, out.ClaimSlot) {
		return disallow(out, "semantic claim slot is incompatible with memory category"), nil
	}
	source := normalizeText(in.SourceUserText)
	if in.Provenance == ProvenanceUserStatement && in.Category == CategoryIdentity && out.Importance < 3 {
		out.Importance = 3
	}

	modelAssessed := in.Mode == ModeAgentSave || in.Mode == ModeBackgroundPattern
	evidenceContext, ok := uniqueEvidenceContext(source, out.Evidence)
	if modelAssessed {
		ok = strings.Contains(source, out.Evidence)
	}
	if !ok {
		return disallow(out, "evidence is not an exact quote from normalized source user text"), nil
	}
	out.Sensitivity = maxSensitivity(out.Sensitivity, ClassifySensitivity(out.Statement+" "+out.Evidence, out.Category))
	if modelAssessed {
		return classifyEligible(out), nil
	}
	if in.Context == ContextHypothetical || in.Context == ContextQuotation {
		return disallow(out, "hypothetical and quoted content is not user memory"), nil
	}
	if containsPromptInjection(source) || containsPromptInjection(out.Statement) || containsPromptInjection(out.Evidence) {
		return disallow(out, "instruction-like content cannot become user memory"), nil
	}
	if containsCredentialMaterial(evidenceContext) || containsCredentialMaterial(out.Statement) || containsCredentialMaterial(out.Evidence) || containsCredentialMaterial(in.ClaimValue) {
		return disallow(out, "credential material cannot become user memory"), nil
	}
	if containsInstructionLikeContent(evidenceContext) || containsInstructionLikeContent(out.Statement) || containsInstructionLikeContent(in.ClaimSlot) || containsInstructionLikeContent(in.ClaimValue) {
		return disallow(out, "instruction, policy, authorization, or capability content cannot become user memory"), nil
	}
	if in.Provenance == ProvenanceThirdParty || in.Provenance == ProvenancePublicSource || in.Provenance == ProvenanceToolOutput {
		return disallow(out, "external facts cannot become tenant memory"), nil
	}
	if in.Provenance == ProvenanceUserStatement {
		if !startsWithDirectUserMarker(out.Evidence) {
			return disallow(out, "user-statement evidence must begin with a direct first-person marker"), nil
		}
		if !hasMeaningfulDirectEvidence(out.Evidence) {
			return disallow(out, "user-statement evidence lacks a meaningful first-person fact"), nil
		}
		if hasUnsafeFactualFraming(out.Evidence) || hasUnsafeFactualFramingWithQuestion(evidenceContext, isIndependentQuestionEvidence(evidenceContext, out.Evidence)) {
			return disallow(out, "interrogative, negative, obsolete, or uncertain evidence is not a direct user fact"), nil
		}
		if isQuotedOrReported(out.Evidence) || isQuotedOrReported(evidenceContext) {
			return disallow(out, "quoted or reported speech is not a direct user fact"), nil
		}
		if isHypotheticalOrConditional(out.Evidence) || isHypotheticalOrConditional(evidenceContext) {
			return disallow(out, "hypothetical or conditional content is not user memory"), nil
		}
		if isThirdPartyCentered(out.Evidence, in.Category, out.ClaimSlot) || isThirdPartyCentered(evidenceContext, in.Category, out.ClaimSlot) {
			return disallow(out, "evidence describes a third party rather than the user"), nil
		}
		if hasPublicAttribution(out.Evidence) || hasPublicAttribution(evidenceContext) {
			return disallow(out, "publicly attributed content is not a private user fact"), nil
		}
		if !isCanonicalUserStatement(out.Statement) {
			return disallow(out, "direct fact statement must be a concise third-person user statement"), nil
		}
		if !directStatementGrounded(out.Statement, out.Evidence, in.Category) {
			if hasCompetingDirectFacts(out.Evidence) {
				return disallow(out, "direct fact statement is not lexically grounded in unambiguous exact evidence"), nil
			}
			out.Statement = directEvidenceStatement(out.Evidence)
			if strings.TrimSpace(in.ClaimValue) == "" {
				out.ClaimSlot, out.ClaimValue = NormalizeClaimIdentity(in.Category, in.ClaimSlot, "", out.Statement)
			}
		}
	}
	if in.Provenance == ProvenanceModelInference {
		if source != out.Evidence {
			return disallow(out, "model inference requires exact whole-turn evidence"), nil
		}
		pacmanMapping := isPacmanArchMapping(source, out.Statement, out.ClaimValue)
		if hasUnsafeInferenceFraming(source, pacmanMapping) || isQuotedOrReported(source) || isHypotheticalOrConditional(source) || isThirdPartyCentered(source, in.Category, out.ClaimSlot) || hasPublicAttribution(source) || containsInstructionLikeContent(source) || containsInstructionLikeContent(out.Statement) {
			return disallow(out, "model inference source or statement is not eligible user evidence"), nil
		}
		if !isUserCenteredInferenceSource(source) && !pacmanMapping {
			return disallow(out, "model inference source is not user-centered"), nil
		}
		if !isQualifiedInferenceStatement(out.Statement) {
			return disallow(out, "model inference statement must be user-centered and governed by cautious qualification"), nil
		}
		if !hasInferenceRelevance(source, out.Statement) {
			return disallow(out, "model inference is not lexically relevant to its source"), nil
		}
	}
	claimGroundingValue := in.ClaimValue
	if strings.TrimSpace(claimGroundingValue) == "" {
		claimGroundingValue = out.ClaimValue
	}
	claimGroundingText := out.Statement + " " + out.Evidence
	claimGroundingReason := "claim value is not lexically grounded in statement or evidence"
	if in.Provenance == ProvenanceUserStatement {
		claimGroundingText = out.Evidence
		claimGroundingReason = "claim value is not lexically grounded in exact evidence"
	}
	if !claimValueGrounded(claimGroundingValue, claimGroundingText) {
		return disallow(out, claimGroundingReason), nil
	}
	if in.Mode == ModeExplicitRemember {
		remembered, ok := ParseExplicitRemember(in.SourceUserText)
		if !ok || !strings.Contains(normalizeText(remembered), out.Evidence) {
			return disallow(out, "explicit mode requires an exact remember phrase containing the evidence"), nil
		}
		if out.Confidence < explicitConfidenceFloor {
			out.Confidence = explicitConfidenceFloor
		}
	}

	return classifyEligible(out), nil
}

func classifyEligible(out CandidateOutput) CandidateOutput {
	if out.Confidence < minimumActiveConfidence {
		out.Reason = "confidence is below the active memory threshold"
		return out
	}
	if out.Context == ContextTemporaryState {
		return approveShortTerm(out)
	}
	out.Approval = ApprovalApproved
	if out.Provenance == ProvenanceModelInference {
		out.Decision = DecisionInferredActive
		out.Reason = "whole-turn model inference meets the active memory threshold"
	} else {
		out.Decision = DecisionAutomatic
		out.Reason = "direct user fact meets the active memory threshold"
	}
	return out
}

func containsCredentialMaterial(value string) bool {
	for _, pattern := range credentialTokenPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	for _, match := range namedCredentialMaterialPattern.FindAllStringSubmatch(value, -1) {
		material := strings.ToLower(strings.Trim(strings.TrimSpace(match[2]), `"'<>[]{}().`))
		switch material {
		case "disabled", "none", "redacted":
			continue
		default:
			return true
		}
	}
	return false
}

// ClassifySensitivity derives the minimum sensitivity from canonical content;
// callers may raise but never lower this classification.
func ClassifySensitivity(statement string, category Category) Sensitivity {
	lower := strings.ToLower(normalizeText(statement))
	if category == CategoryIdentity {
		return SensitivityIdentityOrContact
	}
	for _, marker := range []string{"my name", "user's name", "phone", "email", "address", "contact", "social security", "passport", "birthday", "date of birth", "born", "home location", "where i live", "where the user lives"} {
		if strings.Contains(lower, marker) {
			return SensitivityIdentityOrContact
		}
	}
	hasHighImpactDirective := false
	for _, marker := range []string{"always", "never", "must", "without asking", "do not question", "ignore"} {
		if strings.Contains(lower, marker) {
			hasHighImpactDirective = true
			break
		}
	}
	if hasHighImpactDirective {
		for _, marker := range []string{" you ", " reply", " respond", " answer", " question me", " ask me", " talk to me", " call me"} {
			if strings.Contains(" "+lower+" ", marker) {
				return SensitivityHighImpactInteraction
			}
		}
	}
	return SensitivityLow
}

func maxSensitivity(a, b Sensitivity) Sensitivity {
	rank := func(value Sensitivity) int {
		switch value {
		case SensitivityHighImpactInteraction:
			return 3
		case SensitivityIdentityOrContact:
			return 2
		default:
			return 1
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// ParseExplicitRemember recognizes only the supported explicit intent forms and
// returns the text following the phrase. Matching is case-insensitive.
func ParseExplicitRemember(text string) (string, bool) {
	normalized := normalizeText(text)
	lower := strings.ToLower(normalized)
	for _, negative := range []string{"do not remember ", "please do not remember ", "don't remember ", "please don't remember "} {
		if strings.HasPrefix(lower, negative) {
			return "", false
		}
	}
	for _, prefix := range []string{
		"could you please remember that ", "could you remember that ",
		"please don't forget that ", "don't forget that ",
		"please correct my memory: ", "correct my memory: ",
		"please update your memory: ", "update your memory: ",
		"please remember that ", "remember that ", "remember this: ",
		"please remember: ", "remember: ", "please remember ", "remember ",
	} {
		if strings.HasPrefix(lower, prefix) {
			value := strings.TrimSpace(normalized[len(prefix):])
			if strings.EqualFold(value, "that") || strings.EqualFold(value, "this") {
				return "", false
			}
			return value, value != ""
		}
	}
	return "", false
}

func formationTokens(value string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, field := range contentWords(value) {
		if formationStopWords[field] {
			continue
		}
		stem := formationStem(field)
		if len([]rune(stem)) >= 2 {
			tokens[stem] = struct{}{}
		}
	}
	return tokens
}

func hasMeaningfulDirectEvidence(evidence string) bool {
	return len(formationTokens(evidence)) > 0
}

var formationStopWords = map[string]bool{
	"the": true, "user": true, "users": true, "i": true, "my": true, "me": true,
	"we": true, "our": true, "us": true, "you": true, "your": true, "they": true,
	"their": true, "them": true, "he": true, "his": true, "she": true, "her": true,
	"it": true, "its": true, "is": true, "am": true, "are": true, "was": true,
	"were": true, "be": true, "been": true, "being": true, "has": true, "have": true,
	"had": true, "do": true, "does": true, "did": true, "a": true, "an": true,
	"to": true, "that": true, "this": true, "of": true, "for": true, "in": true,
	"on": true, "at": true, "with": true, "from": true, "as": true, "and": true,
	"or": true, "but": true, "than": true, "oswald": true, "assistant": true, "may": true, "might": true, "possibly": true,
	"likely": true, "appears": true, "seems": true, "could": true, "indicate": true,
}

func startsWithDirectUserMarker(evidence string) bool {
	lower := strings.ToLower(strings.TrimSpace(normalizeText(evidence)))
	for _, marker := range []string{"i ", "i'm ", "i’m ", "i’ve ", "i've ", "i'd ", "i’d ", "i'll ", "i’ll ", "my ", "we ", "we're ", "we’re ", "we've ", "we’ve ", "our "} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

func uniqueEvidenceContext(source, evidence string) (string, bool) {
	sourceRunes, evidenceRunes := []rune(source), []rune(evidence)
	starts := runeSubsliceIndexes(sourceRunes, evidenceRunes)
	if len(starts) != 1 {
		return "", false
	}
	start, end := starts[0], starts[0]+len(evidenceRunes)
	quoteState := quoteStateByRune(sourceRunes)
	if quoteState[start] {
		return "", false
	}
	for start > 0 && !isSentenceBoundary(sourceRunes, start-1, quoteState) {
		start--
	}
	if end == 0 || !isSentenceBoundary(sourceRunes, end-1, quoteState) {
		for end < len(sourceRunes) && !isSentenceBoundary(sourceRunes, end, quoteState) {
			end++
		}
		if end < len(sourceRunes) {
			end++
		}
	}
	return strings.TrimSpace(string(sourceRunes[start:end])), true
}

func isIndependentQuestionEvidence(context, evidence string) bool {
	if !strings.ContainsAny(context, "?？") || strings.ContainsAny(evidence, "?？") {
		return false
	}
	contextRunes, evidenceRunes := []rune(context), []rune(evidence)
	starts := runeSubsliceIndexes(contextRunes, evidenceRunes)
	if len(starts) != 1 {
		return false
	}
	start := starts[0]
	if start == 0 {
		suffix := strings.TrimSpace(string(contextRunes[len(evidenceRunes):]))
		if suffix == "" {
			return false
		}
		first, _ := utf8.DecodeRuneInString(suffix)
		switch first {
		case ',', ';', ':', '—', '–':
			return true
		default:
			return false
		}
	}
	prefix := strings.TrimSpace(string(contextRunes[:start]))
	if prefix == "" {
		return true
	}
	last, _ := utf8.DecodeLastRuneInString(prefix)
	switch last {
	case ',', ';', ':', '—', '–':
		return true
	default:
		return false
	}
}

func runeSubsliceIndexes(value, target []rune) []int {
	if len(target) == 0 || len(target) > len(value) {
		return nil
	}
	var indexes []int
	for i := 0; i+len(target) <= len(value); i++ {
		match := true
		for j := range target {
			if value[i+j] != target[j] {
				match = false
				break
			}
		}
		if match {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func quoteStateByRune(value []rune) []bool {
	state := make([]bool, len(value)+1)
	var ascii, curly, guillemet, single, corner bool
	for i, r := range value {
		state[i] = ascii || curly || guillemet || single || corner
		switch r {
		case '"', '„', '‟':
			ascii = !ascii
		case '“':
			curly = true
		case '”':
			curly = false
		case '«', '‹':
			guillemet = true
		case '»', '›':
			guillemet = false
		case '「', '『':
			corner = true
		case '」', '』':
			corner = false
		case '\'', '‘', '’':
			if !isApostrophe(value, i) {
				single = !single
			}
		}
	}
	state[len(value)] = ascii || curly || guillemet || single || corner
	return state
}

func isApostrophe(value []rune, index int) bool {
	return index > 0 && index+1 < len(value) && unicode.IsLetter(value[index-1]) && unicode.IsLetter(value[index+1])
}

func isSentenceBoundary(value []rune, index int, quoted []bool) bool {
	if index < 0 || index >= len(value) || quoted[index] {
		return false
	}
	switch value[index] {
	case '!', '?', '。', '！', '？', '…':
		return true
	case '.':
		if index > 0 && index+1 < len(value) && unicode.IsDigit(value[index-1]) && unicode.IsDigit(value[index+1]) {
			return false
		}
		return !isAbbreviationPeriod(value, index)
	default:
		return false
	}
}

func isAbbreviationPeriod(value []rune, index int) bool {
	start, end := index, index+1
	for start > 0 && (unicode.IsLetter(value[start-1]) || value[start-1] == '.') {
		start--
	}
	for end < len(value) && (unicode.IsLetter(value[end]) || value[end] == '.') {
		end++
	}
	token := strings.ToLower(string(value[start:end]))
	for _, abbreviation := range []string{"u.s.", "u.k.", "e.g.", "i.e.", "mr.", "mrs.", "ms.", "dr.", "prof.", "etc.", "vs."} {
		if strings.Contains(token, abbreviation) || strings.HasPrefix(abbreviation, token) {
			return true
		}
	}
	return strings.Count(token, ".") >= 2
}

func directStatementGrounded(statement, evidence string, category Category) bool {
	statementTokens := orderedFormationTokens(statement, category == CategoryRelationships && hasExplicitRelationshipNameGrammar(evidence))
	evidenceTokens := orderedFormationTokens(evidence, category == CategoryRelationships && hasExplicitRelationshipNameGrammar(evidence))
	if len(statementTokens) == 0 || len(statementTokens) != len(evidenceTokens) {
		return false
	}
	for i := range statementTokens {
		if statementTokens[i] != evidenceTokens[i] {
			return false
		}
	}
	return true
}

func directEvidenceStatement(evidence string) string {
	return "The user: " + evidence
}

func hasCompetingDirectFacts(evidence string) bool {
	words := contentWords(evidence)
	for _, marker := range [][]string{{"over"}, {"instead", "of"}, {"rather", "than"}, {"versus"}, {"vs"}, {"and"}, {"or"}} {
		if containsWordSequence(words, marker) {
			return true
		}
	}
	return false
}

func orderedFormationTokens(value string, relationshipNameAlias bool) []string {
	var tokens []string
	for _, word := range contentWords(value) {
		if formationStopWords[word] || (relationshipNameAlias && (word == "name" || word == "named")) {
			continue
		}
		stem := formationStem(word)
		if len([]rune(stem)) >= 2 {
			tokens = append(tokens, stem)
		}
	}
	return tokens
}

func hasExplicitRelationshipNameGrammar(value string) bool {
	lower := " " + strings.ToLower(normalizeText(value)) + " "
	return strings.Contains(lower, " is named ") || strings.Contains(lower, "'s name is ") || strings.Contains(lower, "’s name is ")
}

func contentWords(value string) []string {
	return strings.FieldsFunc(strings.ToLower(normalizeText(value)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func formationStem(token string) string {
	token = strings.TrimSuffix(strings.TrimSuffix(token, "'s"), "’s")
	switch {
	case len(token) > 4 && strings.HasSuffix(token, "ies"):
		return strings.TrimSuffix(token, "ies") + "y"
	case len(token) > 4 && strings.HasSuffix(token, "ing"):
		stem := strings.TrimSuffix(token, "ing")
		runes := []rune(stem)
		if len(runes) > 2 && runes[len(runes)-1] == runes[len(runes)-2] {
			stem = string(runes[:len(runes)-1])
		}
		return stem
	case len(token) > 4 && strings.HasSuffix(token, "ed"):
		return strings.TrimSuffix(token, "ed")
	case token == "creator":
		return "creat"
	case len(token) > 3 && strings.HasSuffix(token, "s"):
		return strings.TrimSuffix(token, "s")
	default:
		return token
	}
}

func isThirdPartyCentered(value string, category Category, claimSlot string) bool {
	lower := " " + strings.ToLower(normalizeText(value)) + " "
	for _, marker := range []string{" they ", " she ", " he ", " their ", " her ", " his "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if hasNamedSubjectPredicate(value) {
		return true
	}
	if hasCommonThirdPartySubject(value) {
		return true
	}
	normalized := normalizeText(value)
	trimmedLower := strings.ToLower(normalized)
	for _, role := range []string{
		"sister", "brother", "friend", "partner", "spouse", "wife", "husband", "fiancé", "fiance", "fiancée", "fiancee",
		"colleague", "coworker", "boss", "manager", "roommate", "neighbor", "teacher", "doctor", "client", "customer", "ceo",
		"mother", "father", "mom", "dad", "child", "son", "daughter",
	} {
		marker := "my " + role
		index := strings.Index(trimmedLower, marker)
		if index < 0 {
			continue
		}
		if category == CategoryRelationships && terminalRelationshipIdentity(normalized[index:], len(marker), claimSlot) {
			return false
		}
		return true
	}
	return false
}

func hasCommonThirdPartySubject(value string) bool {
	words := contentWords(value)
	entities := map[string]bool{
		"company": true, "team": true, "organization": true, "organisation": true, "business": true,
		"client": true, "customer": true, "boss": true, "manager": true, "roommate": true,
		"neighbor": true, "teacher": true, "doctor": true, "partner": true, "spouse": true,
		"colleague": true, "coworker": true, "employee": true, "employer": true, "ceo": true,
	}
	predicates := map[string]bool{"is": true, "has": true, "use": true, "uses": true, "runs": true, "works": true, "owns": true, "manages": true, "prefers": true, "likes": true}
	myRoles := map[string]bool{"client": true, "customer": true, "boss": true, "manager": true, "roommate": true, "neighbor": true, "teacher": true, "doctor": true, "partner": true, "spouse": true, "colleague": true, "coworker": true, "ceo": true}
	for i, word := range words {
		if !entities[word] {
			continue
		}
		if word == "manager" && i > 0 && (words[i-1] == "file" || words[i-1] == "package" || words[i-1] == "pacman") {
			continue
		}
		if i > 0 && words[i-1] == "my" && myRoles[word] {
			continue
		}
		for j := i + 1; j < len(words) && j <= i+3; j++ {
			if predicates[words[j]] {
				return true
			}
		}
	}
	return false
}

func terminalRelationshipIdentity(value string, markerLen int, claimSlot string) bool {
	rest := strings.TrimSpace(value[markerLen:])
	lower := strings.ToLower(rest)
	for _, prefix := range []string{"'s name is ", "’s name is ", "is named "} {
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		name := strings.TrimSpace(rest[len(prefix):])
		name = strings.TrimRight(name, ".!?")
		parts := strings.Fields(name)
		if len(parts) == 0 || len(parts) > 4 {
			return false
		}
		for _, part := range parts {
			if !namePart(part) {
				return false
			}
		}
		if !relationshipNameSlot(claimSlot) {
			return false
		}
		return true
	}
	return false
}

func relationshipNameSlot(slot string) bool {
	return strings.HasPrefix(slot, "relationship.") && (strings.Contains(slot, "name") || strings.Contains(slot, "identity"))
}

func hasNamedSubjectPredicate(value string) bool {
	words := strings.Fields(normalizeText(value))
	predicates := []string{"is", "has", "use", "uses", "likes", "lives", "works", "prefers", "said", "says", "reported", "owns", "manages", "runs", "contains"}
	for i := 0; i+1 < len(words); i++ {
		firstWord := strings.Trim(words[i], "\"'“”‘’«».,:;!?")
		first := []rune(firstWord)
		second := strings.ToLower(strings.Trim(words[i+1], "\"'“”‘’«».,:;!?"))
		if len(first) == 0 || !unicode.IsUpper(first[0]) {
			continue
		}
		switch strings.ToLower(firstWord) {
		case "i", "i'm", "i’m", "i've", "i’ve", "i'd", "i’d", "i'll", "i’ll", "my", "we", "we're", "we’re", "we've", "we’ve", "our":
			continue
		}
		for _, verb := range predicates {
			if second == verb {
				return true
			}
		}
		if (strings.HasSuffix(firstWord, "'s") || strings.HasSuffix(firstWord, "’s")) && i+2 < len(words) {
			third := strings.ToLower(strings.Trim(words[i+2], "\"'“”‘’«».,:;!?"))
			for _, verb := range predicates {
				if third == verb {
					return true
				}
			}
		}
	}
	return false
}

func namePart(value string) bool {
	runes := []rune(strings.Trim(value, ","))
	if len(runes) == 0 || !unicode.IsUpper(runes[0]) {
		return false
	}
	lower := strings.ToLower(string(runes))
	for _, descriptor := range []string{"allergic", "diabetic", "to", "peanuts", "and", "or", "likes", "prefers", "uses", "works", "former", "previous", "old", "sick", "healthy", "available", "unavailable", "tall", "short", "blonde", "developer", "engineer", "doctor", "teacher", "manager", "vegetarian", "vegan"} {
		if lower == descriptor {
			return false
		}
	}
	for _, r := range runes[1:] {
		if !unicode.IsLetter(r) && r != '-' && r != '\'' && r != '’' {
			return false
		}
	}
	return true
}

func isQuotedOrReported(value string) bool {
	lower := " " + strings.ToLower(normalizeText(value)) + " "
	if strings.ContainsAny(value, "\"“”„‟«»‹›「」『』") {
		return true
	}
	for _, marker := range []string{" said ", " says ", " told me ", " wrote ", " claimed ", " reported ", " reportedly ", " reports ", " mentioned ", " according to ", " as per ", " i heard ", " i read ", " i think ", " apparently ", " allegedly ", " supposedly "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isHypotheticalOrConditional(value string) bool {
	lower := " " + strings.ToLower(normalizeText(value)) + " "
	for _, marker := range []string{" if ", " when ", " unless ", " assuming ", " suppose ", " supposing ", " imagine ", " hypothetically ", " what if ", " in case ", " would ", " should ", " might ", " could ", " were to ", " maybe ", " perhaps "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func hasPublicAttribution(value string) bool {
	lower := " " + strings.ToLower(normalizeText(value)) + " "
	for _, marker := range []string{" according to ", " wikipedia ", " the news ", " news report ", " an article ", " the article ", " a report ", " a website ", " online ", " internet ", " social media ", " public record ", " publicly "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func containsInstructionLikeContent(value string) bool {
	words := contentWords(value)
	wordSet := make(map[string]bool, len(words))
	for _, word := range words {
		wordSet[word] = true
	}
	for _, forbidden := range []string{"admin", "administrator", "root", "owner", "elevated", "authorize", "authorized", "authorization", "permission", "permissions", "policy", "capability", "capabilities"} {
		if wordSet[forbidden] {
			return true
		}
	}
	for _, forbidden := range []string{"superuser", "sudo", "moderator", "privilege", "privileges", "unrestricted"} {
		if wordSet[forbidden] {
			return true
		}
	}
	for _, phrase := range [][]string{
		{"grant", "access"}, {"can", "access"}, {"tool", "access"}, {"tools", "access"},
		{"allowed", "to"}, {"permit", "to"}, {"run", "tools"}, {"call", "tools"},
		{"execute", "tools"}, {"use", "tools"}, {"grant", "tools"}, {"grant", "tool"},
		{"enable", "tools"}, {"enable", "tool"}, {"can", "execute"}, {"can", "run"}, {"can", "call"},
		{"unrestricted", "access"}, {"delete", "users"}, {"ban", "users"}, {"manage", "users"},
		{"can", "delete"}, {"can", "ban"}, {"can", "manage"},
		{"bypass", "security"}, {"reveal", "secrets"}, {"system", "instruction"},
		{"you", "must"}, {"you", "should"}, {"you", "may"}, {"you", "can"},
	} {
		if containsWordSequence(words, phrase) {
			return true
		}
	}
	if wordSet["you"] && (wordSet["always"] || wordSet["never"] || containsWordSequence(words, []string{"do", "not"})) {
		return true
	}
	return false
}

func containsWordSequence(words, sequence []string) bool {
	for i := 0; i+len(sequence) <= len(words); i++ {
		match := true
		for j := range sequence {
			if words[i+j] != sequence[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func isCanonicalUserStatement(value string) bool {
	lower := strings.ToLower(normalizeText(value))
	return strings.HasPrefix(lower, "the user ") || strings.HasPrefix(lower, "the user's ")
}

func isQualifiedInferenceStatement(value string) bool {
	lower := strings.ToLower(normalizeText(value))
	qualified := false
	for _, prefix := range []string{
		"the user may ", "the user might ", "the user possibly ", "the user likely ",
		"the user appears to ", "the user seems to ", "the user could ",
		"the user's may ", "the user's might ", "the user's possibly ", "the user's likely ",
	} {
		if strings.HasPrefix(lower, prefix) {
			qualified = true
			break
		}
	}
	if !qualified {
		return false
	}
	for _, marker := range []string{" definitely ", " certainly ", " clearly ", " undoubtedly ", " always ", " must ", " will "} {
		if strings.Contains(" "+lower+" ", marker) {
			return false
		}
	}
	for _, marker := range []string{" and is ", " and has ", " and uses ", " and does ", " but is ", " but has ", " but uses ", " but does "} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, marker := range []string{". the user ", "; the user ", ", the user ", ", is ", "; is ", ", has ", "; has "} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

func hasInferenceRelevance(source, statement string) bool {
	sourceTokens := formationTokens(source)
	shared := 0
	sharedToken := ""
	for token := range formationTokens(statement) {
		if _, ok := sourceTokens[token]; ok {
			shared++
			sharedToken = token
		}
	}
	if shared >= 2 {
		return true
	}
	if shared == 1 && (len(sourceTokens) == 1 || sharedToken == "pacman") {
		return true
	}
	return false
}

func isUserCenteredInferenceSource(source string) bool {
	words := contentWords(source)
	for _, word := range words {
		switch word {
		case "i", "my", "me", "we", "our", "us":
			return true
		}
	}
	return false
}

func isPacmanArchMapping(source, statement, claimValue string) bool {
	sourceTokens := formationTokens(source)
	statementTokens := formationTokens(statement)
	claimTokens := formationTokens(strings.ReplaceAll(claimValue, "_", " "))
	_, sourcePacman := sourceTokens["pacman"]
	_, statementPacman := statementTokens["pacman"]
	_, statementArch := statementTokens["arch"]
	_, claimArch := claimTokens["arch"]
	packageFocused := false
	for _, token := range []string{"package", "file", "manager", "software", "linux", "tool"} {
		if _, ok := sourceTokens[token]; ok {
			packageFocused = true
			break
		}
	}
	lower := strings.ToLower(strings.TrimSpace(normalizeText(source)))
	generalQuery := false
	for _, prefix := range []string{"what ", "which ", "how ", "considering ", "looking for ", "recommend ", "recommendation "} {
		if strings.HasPrefix(lower, prefix) {
			generalQuery = true
			break
		}
	}
	return sourcePacman && packageFocused && statementPacman && statementArch && claimArch && (isUserCenteredInferenceSource(source) || generalQuery)
}

func claimValueGrounded(claimValue, groundingText string) bool {
	claimTokens := formationTokens(strings.ReplaceAll(claimValue, "_", " "))
	if len(claimTokens) == 0 {
		return true
	}
	grounding := formationTokens(groundingText)
	for token := range claimTokens {
		if _, ok := grounding[token]; !ok {
			return false
		}
	}
	return true
}

func hasUnsafeFactualFraming(value string) bool {
	return hasUnsafeFactualFramingWithQuestion(value, false)
}

func hasUnsafeInferenceFraming(value string, allowQuestion bool) bool {
	return hasUnsafeFactualFramingWithQuestion(value, allowQuestion)
}

func hasUnsafeFactualFramingWithQuestion(value string, allowQuestion bool) bool {
	normalized := strings.ToLower(normalizeText(value))
	if !allowQuestion && strings.ContainsAny(normalized, "?？") {
		return true
	}
	if strings.Contains(normalized, "can't") || strings.Contains(normalized, "can’t") || strings.Contains(normalized, "cannot") {
		return true
	}
	words := contentWords(normalized)
	for _, word := range words {
		switch word {
		case "no", "not", "never", "don", "doesn", "didn", "isn", "aren", "wasn", "weren", "can", "couldn", "shouldn", "wouldn", "mightn", "unable", "former", "formerly", "previous", "previously", "may", "might", "could", "should", "would", "maybe", "perhaps", "likely", "possibly", "seems", "appears", "until", "once":
			return true
		}
	}
	for _, phrase := range [][]string{{"no", "longer"}, {"used", "to"}, {"did", "i", "mention"}, {"my", "old", "address"}, {"my", "old", "job"}, {"my", "old", "name"}, {"old", "employer"}, {"old", "employment"}} {
		if containsWordSequence(words, phrase) {
			return true
		}
	}
	return false
}

func claimSlotCompatible(category Category, slot string) bool {
	if isFallbackClaimSlotForCategory(category, slot) {
		return true
	}
	switch category {
	case CategoryIdentity:
		return strings.HasPrefix(slot, "identity.")
	case CategoryCommunicationPreferences:
		return strings.HasPrefix(slot, "communication.")
	case CategoryDurablePreferences:
		return strings.HasPrefix(slot, "preference.") || strings.HasPrefix(slot, "durable.")
	case CategoryProjects:
		return strings.HasPrefix(slot, "project.")
	case CategoryRelationships:
		return strings.HasPrefix(slot, "relationship.")
	case CategoryEnvironment:
		return strings.HasPrefix(slot, "environment.")
	case CategoryNotes:
		return strings.HasPrefix(slot, "notes.")
	default:
		return false
	}
}

// ClaimSlotCompatible reports whether a proposed semantic slot belongs to its
// memory category after canonical normalization.
func ClaimSlotCompatible(category Category, slot string) bool {
	return claimSlotCompatible(category, normalizeClaimPart(slot))
}

func isFallbackClaimSlotForCategory(category Category, slot string) bool {
	return slot == normalizeClaimPart(string(category)+".fact")
}

func validateCandidate(in CandidateInput) error {
	fields := []struct {
		name  string
		value string
		max   int
	}{
		{name: "source_user_text", value: in.SourceUserText, max: maxSourceRunes},
		{name: "statement", value: in.Statement, max: maxStatementRunes},
		{name: "evidence", value: in.Evidence, max: maxEvidenceRunes},
		{name: "claim_slot", value: firstNonEmptyClaim(in.ClaimSlot, "default"), max: maxClaimSlotRunes},
		{name: "claim_value", value: firstNonEmptyClaim(in.ClaimValue, "default"), max: maxClaimValueRunes},
	}
	for _, field := range fields {
		name, value := field.name, field.value
		if !utf8.ValidString(value) {
			return invalid(name, "must be valid UTF-8")
		}
		if hasUnsafeRune(value) {
			return invalid(name, "contains a control or bidirectional formatting character")
		}
		if utf8.RuneCountInString(normalizeText(value)) == 0 || utf8.RuneCountInString(value) > field.max {
			return invalid(name, fmt.Sprintf("length must be 1..%d runes", field.max))
		}
	}
	if !validProvenance(in.Provenance) || !validAuthority(in.ClaimedAuthority) || !validSensitivity(in.Sensitivity) ||
		!validMode(in.Mode) || !validScope(in.Scope) || !validCategory(in.Category) || !validContext(in.Context) {
		return invalid("classification", "contains an unknown enum value")
	}
	if math.IsNaN(in.Confidence) || math.IsInf(in.Confidence, 0) || in.Confidence < 0 || in.Confidence > 1 {
		return invalid("confidence", "must be finite and between 0 and 1")
	}
	if in.Importance < 1 || in.Importance > 5 {
		return invalid("importance", "must be between 1 and 5")
	}
	if in.Context == ContextTemporaryState {
		if in.Scope != ScopeShortTerm {
			return invalid("scope", "temporary task state must be short_term")
		}
		if in.TTL != 0 && (in.TTL < minTaskTTL || in.TTL > maxTaskTTL) {
			return invalid("ttl", "temporary TTL must be between 1 hour and 30 days")
		}
	} else {
		if in.Scope != ScopeLongTerm {
			return invalid("scope", "non-temporary memory must be long_term")
		}
		if in.TTL != 0 {
			return invalid("ttl", "only temporary task state may set TTL")
		}
	}
	return nil
}

func normalizeText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func hasUnsafeRune(value string) bool {
	for _, r := range value {
		if (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') || isBidiControl(r) {
			return true
		}
	}
	return false
}

func isBidiControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')
}

func containsPromptInjection(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"ignore previous instructions", "ignore all previous instructions",
		"override previous instructions", "system prompt", "developer message",
		"you are now", "<|system|>", "[system]",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func authorityFor(provenance Provenance) SourceAuthority {
	switch provenance {
	case ProvenanceUserStatement:
		return AuthorityUserDirect
	case ProvenanceModelInference:
		return AuthorityModel
	case ProvenanceThirdParty:
		return AuthorityThirdParty
	case ProvenancePublicSource:
		return AuthorityPublic
	default:
		return AuthorityTool
	}
}

func approveShortTerm(out CandidateOutput) CandidateOutput {
	out.Approval = ApprovalApproved
	out.Decision = DecisionShortTerm
	out.Reason = "temporary task state has bounded retention"
	if out.TTL == 0 {
		out.TTL = defaultTaskTTL
	}
	return out
}

func disallow(out CandidateOutput, reason string) CandidateOutput {
	out.Approval = ApprovalRejected
	out.Decision = DecisionDisallowed
	out.Reason = reason
	return out
}

func invalid(field, reason string) error {
	return fmt.Errorf("%w: %s %s", errInvalidCandidate, field, reason)
}

// NormalizeClaimIdentity returns the canonical slot/value identity used by
// memory formation.
func NormalizeClaimIdentity(category Category, slot, value, statement string) (string, string) {
	slot = normalizeClaimPart(slot)
	if slot == "" {
		slot = normalizeClaimPart(string(category) + ".fact")
	}
	value = normalizeClaimValue(value)
	if value == "" {
		value = normalizeClaimValue(statement)
	}
	if slot == "environment.linux_distribution" || slot == "environment.os_family" {
		switch value {
		case "arch", "arch_linux", "archlinux", "arch_family", "pacman_based", "pacman_based_linux":
			value = "arch_family"
		}
	}
	slot = truncateClaimRunes(slot, maxClaimSlotRunes)
	value = truncateClaimRunes(value, maxClaimValueRunes)
	return slot, value
}

func normalizeClaimPart(value string) string {
	return normalizeClaimToken(value, true)
}

func normalizeClaimValue(value string) string {
	return normalizeClaimToken(value, false)
}

func normalizeClaimToken(value string, allowDot bool) string {
	var out []rune
	underscore := false
	for _, r := range strings.ToLower(normalizeText(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || (allowDot && r == '.') {
			out = append(out, r)
			underscore = false
		} else if !underscore && len(out) > 0 {
			out = append(out, '_')
			underscore = true
		}
	}
	return strings.Trim(string(out), "_")
}

func firstNonEmptyClaim(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func truncateClaimRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimRight(string(runes[:limit]), "_")
}

func validProvenance(v Provenance) bool {
	return v == ProvenanceUserStatement || v == ProvenanceModelInference || v == ProvenanceThirdParty || v == ProvenancePublicSource || v == ProvenanceToolOutput
}

func validAuthority(v SourceAuthority) bool {
	return v == AuthorityUserDirect || v == AuthorityModel || v == AuthorityThirdParty || v == AuthorityPublic || v == AuthorityTool
}

func validSensitivity(v Sensitivity) bool {
	return v == SensitivityLow || v == SensitivityIdentityOrContact || v == SensitivityHighImpactInteraction
}

func validMode(v FormationMode) bool {
	return v == ModeAutomaticExtraction || v == ModePreCompactionExtraction || v == ModeExplicitRemember || v == ModeAgentSave || v == ModeBackgroundPattern
}

func validScope(v Scope) bool { return v == ScopeShortTerm || v == ScopeLongTerm }

func validCategory(v Category) bool {
	return v == CategoryIdentity || v == CategoryCommunicationPreferences || v == CategoryDurablePreferences || v == CategoryProjects || v == CategoryRelationships || v == CategoryEnvironment || v == CategoryNotes
}

func validContext(v ContentContext) bool {
	return v == ContextDirectAssertion || v == ContextTemporaryState || v == ContextHypothetical || v == ContextQuotation
}
