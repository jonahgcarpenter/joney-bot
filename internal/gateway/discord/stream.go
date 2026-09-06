package discord

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

const (
	discordStreamEditInterval     = 800 * time.Millisecond
	discordStreamCursor           = " ▉"
	discordThinkingRenderLimit    = 2000
	discordThinkingRetentionLimit = 8000
	discordToolArgumentValueLimit = 120
	discordToolStatusRenderLimit  = 2000
)

type discordStreamEvent struct {
	chunk     *agent.StreamChunk
	response  *agent.Response
	errorText string
	result    chan error
	abort     bool
}

type discordToolStatus struct {
	name      string
	running   string
	completed string
	failed    string
	done      bool
	isError   bool
}

type discordLifecycleMessage struct {
	id          string
	lastDisplay string
}

type discordResponseStream struct {
	responder    *runtimeResponder
	events       chan discordStreamEvent
	startOnce    sync.Once
	editInterval time.Duration
	terminalMu   sync.Mutex
	terminal     bool
}

func newDiscordResponseStream(responder *runtimeResponder) *discordResponseStream {
	return &discordResponseStream{
		responder:    responder,
		events:       make(chan discordStreamEvent, 128),
		editInterval: discordStreamEditInterval,
	}
}

func (s *discordResponseStream) start() {
	s.startOnce.Do(func() { go s.run() })
}

func (s *discordResponseStream) Push(chunk agent.StreamChunk) {
	s.terminalMu.Lock()
	terminal := s.terminal
	s.terminalMu.Unlock()
	if terminal {
		return
	}
	s.start()
	event := discordStreamEvent{chunk: &chunk}
	if len(chunk.Attachments) > 0 {
		event.result = make(chan error, 1)
	}
	s.events <- event
	if event.result != nil {
		if err := <-event.result; err != nil {
			s.responder.gateway.log().Warn("gateway.attachment.stream_failed", "failed to deliver streamed discord attachment",
				config.F("request_id", s.responder.requestID), config.F("status", "degraded"), config.ErrorField(err))
		}
	}
}

func (s *discordResponseStream) Finish(response *agent.Response) error {
	return s.terminalEvent(discordStreamEvent{response: response})
}

func (s *discordResponseStream) Fail(text string) error {
	return s.terminalEvent(discordStreamEvent{errorText: text})
}

func (s *discordResponseStream) Abort() error {
	return s.terminalEvent(discordStreamEvent{abort: true})
}

func (s *discordResponseStream) terminalEvent(event discordStreamEvent) error {
	s.terminalMu.Lock()
	if s.terminal {
		s.terminalMu.Unlock()
		return nil
	}
	s.terminal = true
	s.terminalMu.Unlock()
	s.start()
	event.result = make(chan error, 1)
	s.events <- event
	return <-event.result
}

func (s *discordResponseStream) run() {
	ticker := time.NewTicker(s.editInterval)
	defer ticker.Stop()

	state := discordStreamState{stream: s}
	for {
		select {
		case event := <-s.events:
			if event.chunk != nil {
				var eventErr error
				if len(event.chunk.Attachments) > 0 {
					eventErr = state.deliverAttachments(event.chunk.Attachments)
				}
				state.consume(*event.chunk)
				if event.result != nil {
					event.result <- eventErr
				}
				continue
			}
			if event.response != nil {
				event.result <- state.finish(event.response)
				return
			}
			if event.abort {
				event.result <- state.discardLifecycleMessages()
				return
			}
			if event.result != nil {
				event.result <- state.fail(event.errorText)
				return
			}
		case <-ticker.C:
			state.flush()
		}
	}
}

type discordStreamState struct {
	stream          *discordResponseStream
	active          strings.Builder
	thinking        string
	frozenThinking  string
	messages        []discordLifecycleMessage
	tool            *discordToolStatus
	replyUsed       bool
	replyMessageID  string
	answerStarted   bool
	thinkingStarted bool
	thinkingDirty   bool
	thinkingTrimmed bool
	frozenTrimmed   bool
	previewDisabled bool
	attachmentCount int
	attachmentErr   error
}

func (s *discordStreamState) consume(chunk agent.StreamChunk) {
	switch chunk.Type {
	case agent.ChunkStatus:
		if s.answerStarted || strings.TrimSpace(chunk.Text) == "" {
			return
		}
		s.tool = nil
		s.thinking = ""
		s.frozenThinking = ""
		s.thinkingStarted = false
		s.thinkingDirty = false
		s.thinkingTrimmed = false
		s.frozenTrimmed = false
		s.show(truncateDiscordText(sanitizeDiscordDisplayText(chunk.Text, false), discordToolStatusRenderLimit))
	case agent.ChunkThinking:
		if s.answerStarted {
			return
		}
		if chunk.Text == "" {
			return
		}
		firstThinkingChunk := !s.thinkingStarted
		if !s.thinkingStarted {
			s.thinking = ""
			s.frozenThinking = ""
			s.thinkingTrimmed = false
			s.frozenTrimmed = false
			s.thinkingStarted = true
			s.tool = nil
		}
		s.appendThinking(chunk.Text)
		if firstThinkingChunk {
			s.showThinking()
		}
	case agent.ChunkContent:
		if chunk.Text == "" {
			return
		}
		s.active.WriteString(chunk.Text)
		if !s.answerStarted {
			s.answerStarted = true
			s.thinking = ""
			s.frozenThinking = ""
			s.thinkingStarted = false
			s.thinkingDirty = false
			s.thinkingTrimmed = false
			s.frozenTrimmed = false
			s.tool = nil
			s.show(strings.TrimSpace(discordStreamCursor))
		}
	case agent.ChunkToolCall:
		s.active.Reset()
		s.answerStarted = false
		continuationCleanupErr := s.discardContinuationMessages()
		if s.thinkingStarted {
			s.frozenThinking = latestThinkingParagraph(s.thinking)
			s.frozenTrimmed = s.thinkingTrimmed
		} else {
			s.frozenThinking = ""
			s.frozenTrimmed = false
		}
		s.thinking = ""
		s.thinkingStarted = false
		s.thinkingDirty = false
		s.thinkingTrimmed = false
		if chunk.Tool != nil && chunk.Tool.Name != "" {
			status := discordToolStatusFor(chunk.Tool)
			s.tool = &status
		}
		s.showTool()
		if continuationCleanupErr != nil {
			s.previewDisabled = true
			s.stream.responder.gateway.log().Debug("gateway.stream.preview_failed", "discord lifecycle continuation cleanup degraded", config.F("request_id", s.stream.responder.requestID), config.F("status", "degraded"), config.ErrorField(continuationCleanupErr))
		}
	case agent.ChunkToolResult:
		if chunk.Tool == nil || chunk.Tool.Name == "" {
			return
		}
		if s.tool == nil || s.tool.name != chunk.Tool.Name {
			status := discordToolStatusFor(chunk.Tool)
			s.tool = &status
		}
		s.tool.done = true
		s.tool.isError = chunk.Tool.IsError
		s.frozenThinking = ""
		s.frozenTrimmed = false
		s.showTool()
	}
}

func (s *discordStreamState) appendThinking(text string) {
	s.thinking += text
	runes := []rune(s.thinking)
	if len(runes) > discordThinkingRetentionLimit {
		s.thinking = string(runes[len(runes)-discordThinkingRetentionLimit:])
		s.thinkingTrimmed = true
	}
	s.thinkingDirty = true
}

func (s *discordStreamState) flush() {
	if s.answerStarted {
		s.flushAnswer()
		return
	}
	if s.thinkingDirty {
		s.showThinking()
	}
}

func (s *discordStreamState) showThinking() {
	if s.answerStarted || !s.thinkingStarted {
		return
	}
	paragraph := latestThinkingParagraph(s.thinking)
	if paragraph == "" {
		return
	}
	s.show(formatThinkingParagraph(paragraph, s.thinkingTrimmed, true, discordThinkingRenderLimit))
	s.thinkingDirty = false
}

func (s *discordStreamState) showTool() {
	if s.answerStarted || s.tool == nil {
		return
	}
	status := s.tool.running
	if s.tool.done && s.tool.isError {
		status = s.tool.failed
	} else if s.tool.done {
		status = s.tool.completed
	}
	if !s.tool.done && s.frozenThinking != "" {
		status = composeRunningToolStatus(s.frozenThinking, s.frozenTrimmed, status)
	}
	s.show(truncateDiscordText(status, discordToolStatusRenderLimit))
}

func (s *discordStreamState) flushAnswer() {
	if !s.answerStarted || s.active.Len() == 0 {
		return
	}
	chunks := splitMessage(s.active.String(), 2000)
	for i, chunk := range chunks {
		display := chunk
		if i == len(chunks)-1 {
			display = discordPreviewText(chunk)
		}
		s.showAt(i, display)
		if s.previewDisabled {
			return
		}
	}
}

func (s *discordStreamState) show(content string) {
	s.showAt(0, content)
}

func (s *discordStreamState) showAt(index int, content string) {
	if s.previewDisabled || content == "" || index < 0 || index > len(s.messages) {
		return
	}
	if index < len(s.messages) && content == s.messages[index].lastDisplay {
		return
	}

	var err error
	if index == len(s.messages) {
		messageID, sendErr := s.send(content)
		err = sendErr
		if err == nil {
			s.messages = append(s.messages, discordLifecycleMessage{id: messageID, lastDisplay: content})
		}
	} else {
		err = s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.messages[index].id, content)
		if err == nil {
			s.messages[index].lastDisplay = content
		}
	}
	if err != nil {
		s.previewDisabled = true
		s.stream.responder.gateway.log().Debug("gateway.stream.preview_failed", "discord lifecycle preview degraded", config.F("request_id", s.stream.responder.requestID), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	s.stream.responder.stopTypingIndicator()
}

func discordPreviewText(text string) string {
	limit := 2000 - discordContentLength(discordStreamCursor) - discordContentLength("\n```")
	chunks := splitMessage(text, limit)
	if len(chunks) == 0 {
		return strings.TrimSpace(discordStreamCursor)
	}
	preview := chunks[0]
	if strings.Count(preview, "```")%2 != 0 {
		preview += "\n```"
	}
	return preview + discordStreamCursor
}

func latestThinkingParagraph(text string) string {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	lines := strings.Split(normalized, "\n")
	current := make([]string, 0, len(lines))
	last := ""
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if paragraph := strings.TrimSpace(strings.Join(current, "\n")); paragraph != "" {
				last = paragraph
			}
			current = current[:0]
			continue
		}
		current = append(current, line)
	}
	if paragraph := strings.TrimSpace(strings.Join(current, "\n")); paragraph != "" {
		return paragraph
	}
	return last
}

func formatThinkingParagraph(paragraph string, omitted, cursor bool, limit int) string {
	paragraph = sanitizeDiscordDisplayText(paragraph, true)
	if paragraph == "" || limit <= 0 {
		return ""
	}
	render := func(text string, showOmission bool) string {
		lines := make([]string, 0, strings.Count(text, "\n")+2)
		if showOmission {
			lines = append(lines, "-# [earlier part omitted]")
		}
		for _, line := range strings.Split(text, "\n") {
			lines = append(lines, "-# "+line)
		}
		if cursor && len(lines) > 0 {
			lines[len(lines)-1] += discordStreamCursor
		}
		return strings.Join(lines, "\n")
	}

	if full := render(paragraph, omitted); discordContentLength(full) <= limit {
		return full
	}

	runes := []rune(paragraph)
	low, high := 0, len(runes)-1
	best := ""
	for low <= high {
		mid := low + (high-low)/2
		candidate := strings.TrimLeftFunc(string(runes[mid:]), unicode.IsSpace)
		rendered := render(candidate, true)
		if discordContentLength(rendered) <= limit {
			best = rendered
			high = mid - 1
		} else {
			low = mid + 1
		}
	}
	return best
}

func composeRunningToolStatus(paragraph string, omitted bool, status string) string {
	status = truncateDiscordText(status, discordToolStatusRenderLimit)
	separator := "\n\n"
	thinkingBudget := discordToolStatusRenderLimit - discordContentLength(status) - discordContentLength(separator)
	thinking := formatThinkingParagraph(paragraph, omitted, false, thinkingBudget)
	if thinking == "" {
		return status
	}
	return thinking + separator + status
}

func discordToolStatusFor(tool *agent.ToolStreamPayload) discordToolStatus {
	name := sanitizeDiscordInline(tool.Name)
	query := toolStringArgument(tool.Arguments, "query")
	if tool.WebSearch != nil && tool.WebSearch.Query != "" {
		query = tool.WebSearch.Query
	}

	switch tool.Name {
	case toolnames.UserMemorySave:
		return actionToolStatus(tool.Name, "Staging a memory for delivery", "Staged a memory for post-delivery validation", "Memory staging failed")
	case toolnames.WebFetch:
		completed := "Fetched the requested public page"
		if tool.WebFetch != nil && tool.WebFetch.IsDegraded {
			completed += " with limited extraction"
		}
		return actionToolStatus(tool.Name, "Fetching the requested public page", completed, "Public page fetch failed")
	case toolnames.WebSearch:
		completed := "Searched the web for " + quoteToolDetail(query)
		if tool.WebSearch != nil && tool.WebSearch.IsDegraded {
			completed += " with limited sources"
		}
		return actionToolStatus(tool.Name, "Searching the web for "+quoteToolDetail(query), completed, "Web search failed for "+quoteToolDetail(query))
	case toolnames.CurrentTime:
		timezone := toolStringArgument(tool.Arguments, "timezone")
		if timezone == "" {
			timezone = "the requested timezone"
		}
		return actionToolStatus(tool.Name, "Checking the time in `"+sanitizeDiscordInline(timezone)+"`", "Checked the time in `"+sanitizeDiscordInline(timezone)+"`", "Time lookup failed for `"+sanitizeDiscordInline(timezone)+"`")
	case toolnames.UserMemorySearch:
		detail := memoryFilterDetail(tool.Arguments)
		if query != "" {
			detail = " for " + quoteToolDetail(query) + detail
		}
		return actionToolStatus(tool.Name, "Searching your memories"+detail, "Searched your memories"+detail, "Memory search failed"+detail)
	case toolnames.UserMemoryList:
		detail := memoryFilterDetail(tool.Arguments)
		return actionToolStatus(tool.Name, "Listing your memories"+detail, "Listed your memories"+detail, "Memory listing failed"+detail)
	case toolnames.SessionTranscriptSearch:
		return actionToolStatus(tool.Name, "Searching this conversation for "+quoteToolDetail(query), "Searched this conversation for "+quoteToolDetail(query), "Conversation search failed for "+quoteToolDetail(query))
	case toolnames.GlobalMemorySearch:
		if tool.GlobalMemory != nil && tool.GlobalMemory.Query != "" {
			query = tool.GlobalMemory.Query
		}
		return actionToolStatus(tool.Name, "Searching Oswald memory for "+quoteToolDetail(query), "Searched Oswald memory for "+quoteToolDetail(query), "Oswald memory search failed for "+quoteToolDetail(query))
	default:
		detail := formatMCPToolArguments(tool.Arguments)
		return actionToolStatus(tool.Name, "Running `"+name+"`"+detail, "Completed `"+name+"`"+detail, "Failed `"+name+"`"+detail)
	}
}

func actionToolStatus(name, running, completed, failed string) discordToolStatus {
	return discordToolStatus{
		name:      name,
		running:   truncateDiscordText(running+"...", discordToolStatusRenderLimit),
		completed: truncateDiscordText(completed+".", discordToolStatusRenderLimit),
		failed:    truncateDiscordText(failed+".", discordToolStatusRenderLimit),
	}
}

func memoryFilterDetail(args map[string]interface{}) string {
	filters := make([]string, 0, 2)
	if scope := toolStringArgument(args, "scope"); scope != "" {
		filters = append(filters, sanitizeDiscordInline(scope))
	}
	if category := toolStringArgument(args, "category"); category != "" {
		filters = append(filters, sanitizeDiscordInline(category))
	}
	if len(filters) == 0 {
		return ""
	}
	return " in " + strings.Join(filters, "/")
}

func toolStringArgument(args map[string]interface{}, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func quoteToolDetail(value string) string {
	value = sanitizeDiscordDisplayText(value, false)
	if value == "" {
		value = "the requested information"
	}
	return strconv.Quote(truncateDiscordText(value, discordToolArgumentValueLimit))
}

func formatMCPToolArguments(args map[string]interface{}) string {
	keys := make([]string, 0, len(args))
	for key := range args {
		if !isSensitiveToolArgument(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value, ok := formatToolArgumentValue(args[key])
		if !ok {
			continue
		}
		parts = append(parts, sanitizeDiscordInline(key)+"="+value)
	}
	if len(parts) == 0 {
		return ""
	}
	return " with " + strings.Join(parts, ", ")
}

func formatToolArgumentValue(value interface{}) (string, bool) {
	switch typed := value.(type) {
	case string:
		return quoteToolDetail(typed), true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32), true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case []string:
		values := make([]string, 0, min(len(typed), 4))
		for i, item := range typed {
			if i == 4 {
				break
			}
			values = append(values, quoteToolDetail(item))
		}
		return "[" + strings.Join(values, ", ") + "]", true
	case []interface{}:
		values := make([]string, 0, min(len(typed), 4))
		for i, item := range typed {
			if i == 4 {
				break
			}
			formatted, ok := formatToolArgumentValue(item)
			if !ok {
				return "", false
			}
			values = append(values, formatted)
		}
		return "[" + strings.Join(values, ", ") + "]", true
	default:
		return "", false
	}
}

func isSensitiveToolArgument(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(normalized)
	for _, fragment := range []string{"auth", "bearer", "credential", "password", "passwd", "passphrase", "secret", "token", "cookie", "header", "key", "jwt", "session"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func sanitizeDiscordInline(value string) string {
	value = sanitizeDiscordDisplayText(value, false)
	return strings.ReplaceAll(value, "`", "'")
}

func sanitizeDiscordDisplayText(value string, preserveNewlines bool) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' && preserveNewlines {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	if !preserveNewlines {
		value = strings.Join(strings.Fields(value), " ")
	}
	value = strings.ReplaceAll(value, "@", "@\u200b")
	return strings.TrimSpace(value)
}

func truncateDiscordText(value string, limit int) string {
	if discordContentLength(value) <= limit {
		return value
	}
	if limit <= 3 {
		return ""
	}
	remaining := limit - 3
	var result strings.Builder
	for _, r := range value {
		units := 1
		if r > 0xffff {
			units = 2
		}
		if units > remaining {
			break
		}
		result.WriteRune(r)
		remaining -= units
	}
	return result.String() + "..."
}

func (s *discordStreamState) finish(response *agent.Response) error {
	attachmentErr := s.deliverRemainingAttachments(response.Attachments)
	responseText := response.Response
	chunks := splitMessage(responseText, 2000)
	log := s.stream.responder.gateway.log()
	log.Debug("gateway.response.prepared", "prepared discord response", config.F("request_id", s.stream.responder.requestID), config.F("chunk_count", len(chunks)), config.F("response_chars", len(responseText)), config.F("model", response.Model))

	if len(chunks) == 0 {
		return errors.Join(attachmentErr, s.discardLifecycleMessages())
	}

	sentCount := 0
	var lifecycleErr error
	finalizedCount := min(len(s.messages), len(chunks))
	var finalEditErr error
	for i := 0; i < finalizedCount; i++ {
		if s.messages[i].lastDisplay == chunks[i] {
			continue
		}
		if err := s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.messages[i].id, chunks[i]); err != nil {
			finalEditErr = fmt.Errorf("edit lifecycle message %d: %w", i+1, err)
			break
		}
		s.messages[i].lastDisplay = chunks[i]
	}
	if finalEditErr != nil {
		if deleteErr := s.discardLifecycleMessages(); deleteErr != nil {
			lifecycleErr = fmt.Errorf("finalize discord lifecycle messages: %v; delete failed: %w", finalEditErr, deleteErr)
		}
		finalizedCount = 0
		log.Debug("gateway.stream.final_edit_failed", "discord final lifecycle edit failed; sending authoritative response separately", config.F("request_id", s.stream.responder.requestID), config.F("status", "degraded"), config.ErrorField(finalEditErr))
	} else {
		for i := len(s.messages) - 1; i >= len(chunks); i-- {
			if err := s.delete(s.messages[i].id, true); err != nil {
				lifecycleErr = errors.Join(lifecycleErr, fmt.Errorf("delete surplus discord lifecycle message %d: %w", i+1, err))
			}
		}
		if len(s.messages) > len(chunks) {
			s.messages = s.messages[:len(chunks)]
		}
		for i := 0; i < finalizedCount; i++ {
			s.remember(s.messages[i].id, chunks[i])
			sentCount++
		}
	}

	for i, chunk := range chunks[finalizedCount:] {
		messageID, err := s.send(chunk)
		if err != nil {
			log.Debug("gateway.send.failed", "failed to send discord response chunk", config.F("request_id", s.stream.responder.requestID), config.F("chunk_index", finalizedCount+i+1), config.ErrorField(err))
			return errors.Join(attachmentErr, err)
		}
		s.remember(messageID, chunk)
		sentCount++
	}
	if err := errors.Join(attachmentErr, lifecycleErr); err != nil {
		return err
	}
	log.Debug("gateway.response.sent", "sent discord response", config.F("request_id", s.stream.responder.requestID), config.F("chunk_count", sentCount), config.F("status", "ok"))
	return nil
}

func (s *discordStreamState) deliverRemainingAttachments(attachments []media.OutputAttachment) error {
	if s.attachmentErr != nil {
		return s.attachmentErr
	}
	if err := media.ValidateOutputAttachments(attachments); err != nil {
		return err
	}
	if s.attachmentCount > len(attachments) {
		return fmt.Errorf("streamed attachment count exceeds final attachment count")
	}
	return s.deliverAttachments(attachments[s.attachmentCount:])
}

func (s *discordStreamState) deliverAttachments(attachments []media.OutputAttachment) error {
	if s.attachmentErr != nil {
		return s.attachmentErr
	}
	if err := media.ValidateOutputAttachments(attachments); err != nil {
		s.attachmentErr = err
		return err
	}
	for i := range attachments {
		replyToID := ""
		if !s.replyUsed {
			replyToID = s.stream.responder.replyToID
		}
		if _, err := s.stream.responder.gateway.sendCommandAttachment(s.stream.responder.channelID, commands.Result{Attachments: attachments[i : i+1]}, replyToID); err != nil {
			s.attachmentErr = err
			return err
		}
		s.replyUsed = true
		s.attachmentCount++
	}
	return nil
}

func (s *discordStreamState) fail(text string) error {
	var lifecycleErr error
	if len(s.messages) > 0 {
		if err := s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.messages[0].id, text); err == nil {
			for i := len(s.messages) - 1; i > 0; i-- {
				if deleteErr := s.delete(s.messages[i].id, true); deleteErr != nil {
					lifecycleErr = errors.Join(lifecycleErr, fmt.Errorf("delete surplus discord lifecycle message %d: %w", i+1, deleteErr))
				}
			}
			s.remember(s.messages[0].id, text)
			return lifecycleErr
		} else if deleteErr := s.discardLifecycleMessages(); deleteErr != nil {
			lifecycleErr = fmt.Errorf("replace discord lifecycle message with error: edit failed: %v; delete failed: %w", err, deleteErr)
		}
	}
	messageID, err := s.send(text)
	if err != nil {
		return err
	}
	s.remember(messageID, text)
	return lifecycleErr
}

func (s *discordStreamState) discardLifecycleMessages() error {
	messages := s.messages
	s.messages = nil
	var deleteErr error
	for i := len(messages) - 1; i >= 0; i-- {
		if err := s.delete(messages[i].id, true); err != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("delete lifecycle message %d: %w", i+1, err))
		}
	}
	for _, message := range messages {
		if message.id == s.replyMessageID {
			s.replyUsed = false
			s.replyMessageID = ""
			break
		}
	}
	return deleteErr
}

func (s *discordStreamState) discardContinuationMessages() error {
	if len(s.messages) <= 1 {
		return nil
	}
	kept := s.messages[:1]
	var deleteErr error
	for i, message := range s.messages[1:] {
		if err := s.delete(message.id, false); err != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("delete continuation lifecycle message %d: %w", i+2, err))
			kept = append(kept, message)
		}
	}
	s.messages = kept
	return deleteErr
}

func (s *discordStreamState) send(content string) (string, error) {
	replyToID := ""
	if !s.replyUsed {
		replyToID = s.stream.responder.replyToID
	}
	messageID, err := s.stream.responder.gateway.sendMessage(s.stream.responder.channelID, content, replyToID, s.stream.responder.gateway.log().With(config.F("request_id", s.stream.responder.requestID)))
	if err == nil {
		if !s.replyUsed {
			s.replyMessageID = messageID
		}
		s.replyUsed = true
	}
	return messageID, err
}

func (s *discordStreamState) delete(messageID string, releaseReply bool) error {
	err := s.stream.responder.gateway.deleteMessage(s.stream.responder.channelID, messageID)
	if err == nil && releaseReply && messageID == s.replyMessageID {
		s.replyUsed = false
		s.replyMessageID = ""
	}
	return err
}

func (s *discordStreamState) remember(messageID, text string) {
	if messageID == "" {
		return
	}
	r := s.stream.responder
	r.gateway.rememberReply(messageID, replyContext{
		SessionKey: r.sessionKey, ChannelID: r.channelID, SenderID: r.authorID,
		DisplayName: "Oswald", Text: text, IsFromBot: true, CreatedAt: time.Now(),
	})
}
