package websearch

import (
	"errors"
	"html"
	"net/netip"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxResults             = 8
	maxCandidates          = 50
	maxResponseBytes       = 2 << 20
	maxQueryRunes          = 400
	maxQueryWords          = 50
	maxURLBytes            = 2048
	maxTitleRunes          = 240
	maxSnippetRunes        = 1200
	maxEngineNames         = 8
	maxEngineNameRunes     = 64
	maxUnresponsiveEngines = 8
)

var trackingParameters = map[string]struct{}{
	"fbclid": {}, "gclid": {}, "dclid": {}, "msclkid": {},
	"mc_cid": {}, "mc_eid": {}, "_ga": {}, "_gl": {},
}

type searchCandidate struct {
	Title              string
	URL                string
	Content            string
	Score              float64
	Engine             string
	Engines            []string
	Positions          []int
	Category           string
	PublishedDate      string
	PreserveWhitespace bool
}

func validateQuery(query string) error {
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) > maxQueryRunes {
		return errors.New("search query exceeded 400 characters or was invalid UTF-8")
	}
	for _, r := range query {
		if unicode.IsControl(r) {
			return errors.New("search query contains disallowed control characters")
		}
	}
	if strings.TrimSpace(query) == "" {
		return errors.New("search query was empty")
	}
	if len(strings.Fields(query)) > maxQueryWords {
		return errors.New("search query exceeded 50 words")
	}
	return nil
}

func normalizeCandidates(candidates []searchCandidate, unresponsiveEngines []string) SearchResponse {
	response := SearchResponse{
		Notice:              toolNotice,
		UnresponsiveEngines: unresponsiveEngines,
		Results:             make([]SearchResult, 0, maxResults),
		Stats: CandidateStats{
			CandidateCount: len(candidates),
		},
	}
	response.Degraded = len(response.UnresponsiveEngines) > 0
	seenURLs := make(map[string]int)
	hostCounts := make(map[string]int)
	inspect := min(len(candidates), maxCandidates)
	response.Stats.InspectedCount = inspect

	for _, candidate := range candidates[:inspect] {
		result, ok := normalizeResult(candidate)
		if !ok {
			response.Stats.FilteredCount++
			continue
		}
		if index, duplicate := seenURLs[result.URL]; duplicate {
			response.Results[index].Engines = mergeNames(response.Results[index].Engines, result.Engines, maxEngineNames)
			response.Stats.DuplicateCount++
			continue
		}
		if hostCounts[result.Domain] >= 2 {
			response.Stats.FilteredCount++
			continue
		}
		if len(response.Results) >= maxResults {
			continue
		}
		seenURLs[result.URL] = len(response.Results)
		hostCounts[result.Domain]++
		response.Results = append(response.Results, result)
	}
	return response
}

func normalizeResult(candidate searchCandidate) (SearchResult, bool) {
	title := truncateRunes(cleanText(candidate.Title), maxTitleRunes)
	if title == "" || len(candidate.URL) > maxURLBytes {
		return SearchResult{}, false
	}
	parsed, err := url.Parse(candidate.URL)
	if err == nil {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
	}
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return SearchResult{}, false
	}
	domain := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if domain == "" || rejectedLiteralIP(domain) {
		return SearchResult{}, false
	}
	parsed.Host = canonicalHost(parsed, domain)
	parsed.Fragment = ""
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return SearchResult{}, false
	}
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") {
			query.Del(key)
			continue
		}
		if _, tracked := trackingParameters[lower]; tracked {
			query.Del(key)
		}
	}
	parsed.RawQuery = query.Encode()
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	canonical := parsed.String()
	if len(canonical) > maxURLBytes {
		return SearchResult{}, false
	}

	engines := mergeNames(nil, append([]string{candidate.Engine}, candidate.Engines...), maxEngineNames)
	snippet := cleanText(candidate.Content)
	if candidate.PreserveWhitespace {
		snippet = cleanContextText(candidate.Content)
	}
	snippet = truncateRunes(snippet, maxSnippetRunes)
	return SearchResult{
		Title:       title,
		URL:         canonical,
		Domain:      domain,
		Snippet:     snippet,
		Engines:     engines,
		PublishedAt: truncateRunes(cleanText(candidate.PublishedDate), 64),
		Score:       candidate.Score,
		Category:    truncateRunes(cleanText(candidate.Category), 64),
		Positions:   append([]int(nil), candidate.Positions...),
	}, true
}

func canonicalHost(parsed *url.URL, domain string) string {
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(domain, ":") {
		domain = "[" + domain + "]"
	}
	if port != "" {
		return domain + ":" + port
	}
	return domain
}

func rejectedLiteralIP(host string) bool {
	address, err := netip.ParseAddr(host)
	if err != nil {
		return strings.EqualFold(host, "localhost")
	}
	return address.IsUnspecified() || address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast()
}

func cleanText(value string) string {
	value = html.UnescapeString(value)
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			builder.WriteRune(' ')
			continue
		}
		builder.WriteRune(r)
	}
	return strings.Join(strings.Fields(builder.String()), " ")
}

func cleanContextText(value string) string {
	value = html.UnescapeString(strings.ReplaceAll(value, "\r\n", "\n"))
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			builder.WriteRune(' ')
			continue
		}
		builder.WriteRune(r)
	}
	return strings.TrimSpace(builder.String())
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func mergeNames(existing, incoming []string, limit int) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	out := make([]string, 0, min(limit, len(existing)+len(incoming)))
	for _, raw := range append(append([]string(nil), existing...), incoming...) {
		name := truncateRunes(cleanText(raw), maxEngineNameRunes)
		key := strings.ToLower(name)
		if name == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
		if len(out) == limit {
			break
		}
	}
	return out
}
