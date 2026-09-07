package memory

import (
	"sort"
	"strconv"
	"strings"
)

// ProvenanceLabel describes formation authority without implying verification or
// presenting a model confidence estimate as a calibrated probability.
func ProvenanceLabel(provenance string) string {
	switch provenance {
	case "user_statement":
		return "stated"
	case "model_inference":
		return "inferred"
	default:
		return "unknown"
	}
}

// RenderMarkdown formats entries as compact Markdown for tools and stream payloads.
func RenderMarkdown(intro string, entries []MemoryEntry) string {
	var b strings.Builder
	if strings.TrimSpace(intro) != "" {
		b.WriteString(strings.TrimSpace(intro))
		b.WriteString("\n\n")
	}
	if len(entries) == 0 {
		return strings.TrimSpace(b.String())
	}
	b.WriteString("# User Memory\n")
	byHeading := map[string][]MemoryEntry{}
	for _, entry := range entries {
		heading := displayCategoryName(entry.Scope + " " + entry.Category)
		byHeading[heading] = append(byHeading[heading], entry)
	}
	headings := make([]string, 0, len(byHeading))
	for heading := range byHeading {
		headings = append(headings, heading)
	}
	sort.Strings(headings)
	for _, heading := range headings {
		b.WriteString("\n## ")
		b.WriteString(heading)
		b.WriteString("\n\n")
		for _, entry := range byHeading[heading] {
			b.WriteString("- Statement: ")
			b.WriteString(quoteProfileText(normalizeProfileText(entry.Statement)))
			if context := normalizeProfileText(entry.Context); context != "" {
				b.WriteString("\n\n- Context: ")
				b.WriteString(quoteProfileText(context))
			}
			b.WriteString("\n\n- Memory ID: ")
			b.WriteString(strconv.FormatInt(entry.ID, 10))
			b.WriteString("\n\n- Revision: ")
			b.WriteString(strconv.FormatInt(entry.Revision, 10))
			b.WriteString("\n\n- Claim slot: ")
			b.WriteString(quoteProfileText(entry.ClaimSlot))
			b.WriteString("\n\n- Claim value: ")
			b.WriteString(quoteProfileText(entry.ClaimValue))
			b.WriteString("\n\n- Evidence: ")
			b.WriteString(quoteProfileText(normalizeProfileText(entry.Evidence)))
			b.WriteString("\n\n- Confidence: ")
			b.WriteString(strconv.FormatFloat(clampRecallScore(entry.Confidence), 'f', 4, 64))
			b.WriteString("\n\n- Formation provenance: ")
			b.WriteString(quoteProfileText(normalizeProfileToken(entry.ProvenanceType)))
			b.WriteString("\n\n- Source authority: ")
			b.WriteString(quoteProfileText(normalizeProfileToken(entry.SourceAuthority)))
			b.WriteString("\n\n- Epistemic status: ")
			b.WriteString(quoteProfileText(recallEpistemicStatus(recallAuthorityForEntry(entry), entry.Confidence)))
			b.WriteString("\n\n- Sensitivity: ")
			b.WriteString(quoteProfileText(normalizeProfileToken(entry.Sensitivity)))
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func displayCategoryName(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	parts := strings.Fields(value)
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

// RenderedSections is a sectioned representation of rendered memory content.
type RenderedSections struct {
	Intro    string            `json:"intro,omitempty"`
	Sections map[string]string `json:"sections,omitempty"`
}

// ParseRenderedMarkdown converts rendered memory Markdown into sections.
func ParseRenderedMarkdown(content string) RenderedSections {
	parsed := RenderedSections{Sections: map[string]string{}}
	content = strings.TrimSpace(content)
	if content == "" {
		return parsed
	}
	lines := strings.Split(content, "\n")
	var current string
	var b strings.Builder
	flush := func() {
		if current != "" {
			parsed.Sections[current] = strings.TrimSpace(b.String())
			b.Reset()
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "# User Memory") {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		if current == "" && strings.HasPrefix(strings.TrimSpace(line), "You are speaking with ") {
			parsed.Intro = strings.TrimSpace(line)
			continue
		}
		if current != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	flush()
	if len(parsed.Sections) == 0 {
		parsed.Sections = nil
	}
	return parsed
}
