package opencode

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/usage"
)

// maxTitleLen caps titles derived from a first user message.
const maxTitleLen = 80

// Mapped is one OpenCode session converted into the rows Crush stores.
type Mapped struct {
	// Session is the session row, with ID and ParentSessionID already
	// rewritten to their Crush values.
	Session db.ImportSessionParams
	// Messages are the message rows in conversation order.
	Messages []db.ImportMessageParams
	// Events are the usage ledger rows for the session.
	Events []db.CreateUsageEventParams
	// Children maps a sub-agent session that a task call spawned to the
	// Crush session ID it must be imported under.
	Children map[string]string
	// Dropped counts OpenCode messages and parts that have no Crush
	// equivalent, by kind.
	Dropped map[string]int
	// Todos is the decoded todo list stored on the session row.
	Todos []session.Todo
}

// MapOptions tunes the conversion.
type MapOptions struct {
	// SessionID is the Crush session ID to use. Empty keeps the OpenCode ID.
	SessionID string
	// ParentSessionID is the Crush ID of the parent, or empty for roots.
	ParentSessionID string
	// Usage controls whether usage ledger rows are produced.
	Usage bool
}

// MapSession converts one OpenCode session and its messages.
func MapSession(ctx context.Context, src *Source, sess Session, opts MapOptions) (*Mapped, error) {
	messages, err := src.Messages(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	todos, err := src.Todos(ctx, sess.ID)
	if err != nil {
		return nil, err
	}

	id := opts.SessionID
	if id == "" {
		id = sess.ID
	}
	m := &Mapped{
		Children: map[string]string{},
		Dropped:  map[string]int{},
	}

	var (
		clock          = newClock()
		firstUserText  string
		lastAssistant  *tokens
		summaryID      string
		costSum        float64
		haveCost       bool
		lastTodosInput json.RawMessage
	)

	for _, raw := range messages {
		t, err := decode(src.Generation(), raw)
		if err != nil {
			m.Dropped["undecodable"]++
			continue
		}
		for k, v := range t.dropped {
			m.Dropped[k] += v
		}
		switch t.kind {
		case kindDropped:
			m.Dropped[t.drop]++
			continue
		case kindUser:
			parts, err := userParts(ctx, src, t)
			if err != nil {
				return nil, err
			}
			if len(parts) == 0 {
				m.Dropped["empty-user"]++
				continue
			}
			if firstUserText == "" {
				firstUserText = strings.TrimSpace(t.text)
			}
			parts = append(parts, message.Finish{Reason: "stop"})
			row, err := messageRow(id, t.id, message.User, parts, t, clock, "", "", false)
			if err != nil {
				return nil, err
			}
			m.Messages = append(m.Messages, row)
		case kindHidden:
			if strings.TrimSpace(t.text) == "" {
				m.Dropped["empty-hidden"]++
				continue
			}
			parts := []message.ContentPart{
				message.TextContent{Text: t.text, Hidden: true},
				message.Finish{Reason: "stop"},
			}
			row, err := messageRow(id, t.id, message.User, parts, t, clock, "", "", false)
			if err != nil {
				return nil, err
			}
			m.Messages = append(m.Messages, row)
		case kindShell:
			parts := []message.ContentPart{
				message.ShellCommand{Command: t.command, Output: t.output, ExitCode: t.exitCode},
				message.Finish{Reason: "stop"},
			}
			row, err := messageRow(id, t.id, message.User, parts, t, clock, "", "", false)
			if err != nil {
				return nil, err
			}
			m.Messages = append(m.Messages, row)
		case kindCompaction:
			if strings.TrimSpace(t.text) == "" {
				m.Dropped["empty-compaction"]++
				continue
			}
			finishAt := t.completed
			if finishAt == 0 {
				finishAt = t.created
			}
			parts := []message.ContentPart{
				message.TextContent{Text: t.text},
				message.Finish{Reason: message.FinishReasonEndTurn, Time: finishAt / 1000},
			}
			row, err := messageRow(id, t.id, message.Assistant, parts, t, clock, t.model, t.provider, true)
			if err != nil {
				return nil, err
			}
			m.Messages = append(m.Messages, row)
			summaryID = row.ID
			if opts.Usage && t.hasUsage() {
				m.Events = append(m.Events, usageEvent(id, t, t.cost, t.tokens, finishAt, usage.SourceSummarize))
			}
			if t.hasCost {
				costSum += t.cost
				haveCost = true
			}
		case kindAssistant:
			rows, events, childLinks, todosInput, err := assistantRows(id, t, clock, opts.Usage)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				m.Dropped["empty-assistant"]++
				continue
			}
			m.Messages = append(m.Messages, rows...)
			m.Events = append(m.Events, events...)
			for child, crushID := range childLinks {
				m.Children[child] = crushID
			}
			if todosInput != nil {
				lastTodosInput = todosInput
			}
			if t.summary {
				summaryID = rows[0].ID
			}
			if t.tokens.any() {
				lastAssistant = t.tokens
			} else if n := len(t.steps); n > 0 && t.steps[n-1].tokens.any() {
				lastAssistant = t.steps[n-1].tokens
			}
			if t.hasCost {
				costSum += t.cost
				haveCost = true
			} else {
				for _, step := range t.steps {
					if step.hasCost {
						costSum += step.cost
						haveCost = true
					}
				}
			}
		}
	}

	title := strings.TrimSpace(sess.Title)
	if title == "" {
		title = strings.TrimSpace(sess.Slug)
	}
	if title == "" && firstUserText != "" {
		title = truncate(firstLine(firstUserText), maxTitleLen)
	}
	if title == "" {
		title = "Imported OpenCode session " + sess.ID
	}

	cost := sess.Cost
	if haveCost {
		cost = costSum
	}
	if cost < 0 {
		cost = 0
	}

	var prompt, completion int64
	if lastAssistant != nil {
		prompt = lastAssistant.Input.int64() + lastAssistant.Cache.Read.int64() + lastAssistant.Cache.Write.int64()
		completion = lastAssistant.Output.int64() + lastAssistant.Reasoning.int64()
	}

	m.Todos = sessionTodos(todos, lastTodosInput)
	todosJSON := sql.NullString{}
	if len(m.Todos) > 0 {
		data, err := json.Marshal(m.Todos)
		if err != nil {
			return nil, fmt.Errorf("encode todos: %w", err)
		}
		todosJSON = sql.NullString{String: string(data), Valid: true}
	}

	created := sess.Created / 1000
	updated := sess.Updated / 1000
	if updated < created {
		updated = created
	}

	m.Session = db.ImportSessionParams{
		ID:               id,
		ParentSessionID:  sql.NullString{String: opts.ParentSessionID, Valid: opts.ParentSessionID != ""},
		Title:            title,
		MessageCount:     int64(len(m.Messages)),
		PromptTokens:     max(prompt, 0),
		CompletionTokens: max(completion, 0),
		Cost:             cost,
		SummaryMessageID: sql.NullString{String: summaryID, Valid: summaryID != ""},
		Todos:            todosJSON,
		CreatedAt:        created,
		UpdatedAt:        updated,
	}
	return m, nil
}

// clock hands out strictly increasing second timestamps so that Crush's
// created_at ordering reproduces OpenCode's sequence even when several
// messages share a second.
type clock struct {
	last int64
}

func newClock() *clock { return &clock{} }

// next returns the timestamp for a message created at ms milliseconds.
func (c *clock) next(ms int64) int64 {
	sec := ms / 1000
	if sec <= c.last {
		sec = c.last + 1
	}
	c.last = sec
	return sec
}

func messageRow(sessionID, id string, role message.MessageRole, parts []message.ContentPart, t turn, clock *clock, model, provider string, summary bool) (db.ImportMessageParams, error) {
	data, err := message.MarshalParts(parts)
	if err != nil {
		return db.ImportMessageParams{}, fmt.Errorf("encode parts of %s: %w", id, err)
	}
	created := clock.next(t.created)
	updated := max(t.updated/1000, created)
	row := db.ImportMessageParams{
		ID:        id,
		SessionID: sessionID,
		Role:      string(role),
		Parts:     string(data),
		Model:     sql.NullString{String: model, Valid: model != ""},
		Provider:  sql.NullString{String: provider, Valid: provider != ""},
		CreatedAt: created,
		UpdatedAt: updated,
	}
	if summary {
		row.IsSummaryMessage = 1
	}
	if role == message.Assistant {
		for _, p := range parts {
			if f, ok := p.(message.Finish); ok && f.Time > 0 {
				row.FinishedAt = sql.NullInt64{Int64: f.Time, Valid: true}
			}
		}
	}
	return row, nil
}

// userParts builds the content of a user message, resolving attachments.
func userParts(ctx context.Context, src *Source, t turn) ([]message.ContentPart, error) {
	var parts []message.ContentPart
	text := unquote(t.text)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, message.TextContent{Text: text})
	}
	for _, a := range t.files {
		part, err := attachmentPart(ctx, src, a)
		if err != nil {
			return nil, err
		}
		if part != nil {
			parts = append(parts, part)
		}
	}
	return parts, nil
}

// unquote undoes the JSON string quoting a few OpenCode clients applied to
// prompts before storing them.
func unquote(text string) string {
	if len(text) >= 2 && strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`) {
		var s string
		if err := json.Unmarshal([]byte(text), &s); err == nil {
			return s
		}
	}
	return text
}

func attachmentPart(ctx context.Context, src *Source, a attachment) (message.ContentPart, error) {
	name := a.Name
	if name == "" {
		name = a.Filename
	}
	if name == "" {
		name = "attachment"
	}
	mime := a.Mime

	switch {
	case a.Data != "":
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return message.TextContent{Text: fmt.Sprintf("[attachment %s could not be decoded]", name)}, nil
		}
		return message.BinaryContent{Path: name, MIMEType: mime, Data: data}, nil
	case strings.HasPrefix(a.URI, "media:"):
		blob, err := src.Blob(ctx, strings.TrimPrefix(a.URI, "media:"))
		if err != nil {
			return nil, err
		}
		if blob == nil {
			return message.TextContent{Text: fmt.Sprintf("[attachment %s is no longer available]", name)}, nil
		}
		if mime == "" {
			mime = blob.Mime
		}
		return message.BinaryContent{Path: name, MIMEType: mime, Data: blob.Data}, nil
	}

	uri := a.URL
	if uri == "" {
		uri = a.URI
	}
	switch {
	case strings.HasPrefix(uri, "data:"):
		data, dataMime, ok := decodeDataURL(uri)
		if !ok {
			return message.TextContent{Text: fmt.Sprintf("[attachment %s could not be decoded]", name)}, nil
		}
		if mime == "" {
			mime = dataMime
		}
		return message.BinaryContent{Path: name, MIMEType: mime, Data: data}, nil
	case uri != "" && strings.HasPrefix(mime, "image/"):
		return message.ImageURLContent{URL: uri}, nil
	case uri != "":
		return message.TextContent{Text: fmt.Sprintf("[attachment %s (%s): %s]", name, mime, uri)}, nil
	}
	return nil, nil
}

// decodeDataURL decodes a base64 data: URL into bytes and its media type.
func decodeDataURL(uri string) ([]byte, string, bool) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return nil, "", false
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return nil, "", false
	}
	mime, _, _ := strings.Cut(meta, ";")
	if !strings.Contains(meta, ";base64") {
		return []byte(payload), mime, true
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return nil, "", false
		}
	}
	return data, mime, true
}

// assistantRows converts an assistant turn into its Crush assistant message
// and, when it called tools, the tool-result message that follows it.
func assistantRows(sessionID string, t turn, clock *clock, withUsage bool) (rows []db.ImportMessageParams, events []db.CreateUsageEventParams, children map[string]string, todosInput json.RawMessage, err error) {
	var parts []message.ContentPart

	if len(t.reasoning) > 0 {
		var (
			texts      []string
			start, end int64
		)
		for _, r := range t.reasoning {
			if strings.TrimSpace(r.text) != "" {
				texts = append(texts, r.text)
			}
			if r.start > 0 && (start == 0 || r.start < start) {
				start = r.start
			}
			if r.end > end {
				end = r.end
			}
		}
		if len(texts) > 0 {
			rc := message.ReasoningContent{Thinking: strings.Join(texts, "\n\n")}
			if start > 0 {
				rc.StartedAt = start / 1000
				if end >= start {
					rc.FinishedAt = end / 1000
				} else {
					rc.FinishedAt = rc.StartedAt
				}
			}
			parts = append(parts, rc)
		}
	}

	text := strings.Join(nonEmpty(t.texts), "\n\n")
	if text != "" {
		parts = append(parts, message.TextContent{Text: text})
	}

	var (
		results   []message.ContentPart
		resultsAt int64
	)
	children = map[string]string{}
	for i, item := range t.tools {
		callID := item.id
		if callID == "" {
			callID = fmt.Sprintf("%s-call-%d", t.id, i)
		}
		name, input := translateTool(item.name, item.input)
		parts = append(parts, message.ToolCall{ID: callID, Name: name, Input: input, Finished: true})

		img := singleImage(item)
		content, isErr := toolResultText(item, img == nil)
		result := message.ToolResult{
			ToolCallID: callID,
			Name:       name,
			Content:    content,
			MIMEType:   "text/plain",
			IsError:    isErr,
		}
		if img != nil {
			result.Data = img.data
			result.MIMEType = img.mime
			if strings.TrimSpace(result.Content) == "" {
				result.Content = "Loaded " + img.mime + " content"
			}
		}
		results = append(results, result)
		if item.end > resultsAt {
			resultsAt = item.end
		}
		if name == "agent" {
			if child := childSessionID(item); child != "" {
				children[child] = t.id + "$$" + callID
			}
		}
		if name == "todos" && item.status == "completed" && len(item.input) > 0 {
			todosInput = item.input
		}
	}

	if len(parts) == 0 && t.errMsg == "" {
		return nil, nil, nil, nil, nil
	}

	finishAt := t.completed
	if finishAt == 0 {
		finishAt = t.created
	}
	finish := message.Finish{
		Reason:  finishReason(t, text != "" || len(t.tools) > 0),
		Time:    finishAt / 1000,
		Message: t.errMsg,
	}
	parts = append(parts, finish)

	row, err := messageRow(sessionID, t.id, message.Assistant, parts, t, clock, t.model, t.provider, t.summary)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	rows = append(rows, row)

	if len(results) > 0 {
		results = append(results, message.Finish{Reason: "stop"})
		if resultsAt == 0 {
			resultsAt = finishAt
		}
		toolTurn := turn{created: max(resultsAt, t.created), updated: t.updated}
		toolRow, err := messageRow(sessionID, t.id+"/tools", message.Tool, results, toolTurn, clock, "", "", false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		rows = append(rows, toolRow)
	}

	if withUsage {
		switch {
		case len(t.steps) > 0:
			for _, step := range t.steps {
				if !(step.hasCost && step.cost > 0) && !step.tokens.any() {
					continue
				}
				at := step.at
				if at == 0 {
					at = finishAt
				}
				events = append(events, usageEvent(sessionID, t, step.cost, step.tokens, at, usage.SourceTurn))
			}
		case t.hasUsage():
			events = append(events, usageEvent(sessionID, t, t.cost, t.tokens, finishAt, usage.SourceTurn))
		}
	}
	return rows, events, children, todosInput, nil
}

type inlineImage struct {
	data string
	mime string
}

// singleImage returns the base64 payload of a tool result that consists of
// exactly one inline image, so the viewer can show it the way it shows a
// Crush image read.
func singleImage(item toolItem) *inlineImage {
	if item.status != "completed" || len(item.files) != 1 {
		return nil
	}
	f := item.files[0]
	if !strings.HasPrefix(f.Mime, "image/") || !strings.HasPrefix(f.URI, "data:") {
		return nil
	}
	_, payload, ok := strings.Cut(f.URI, ",")
	if !ok || !strings.Contains(f.URI, ";base64") {
		return nil
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return nil
	}
	return &inlineImage{data: payload, mime: f.Mime}
}

func usageEvent(sessionID string, t turn, cost float64, tok *tokens, atMs int64, source usage.Source) db.CreateUsageEventParams {
	ev := db.CreateUsageEventParams{
		SessionID: sessionID,
		Provider:  t.provider,
		Model:     t.model,
		Source:    string(source),
		Cost:      max(cost, 0),
		CreatedAt: atMs / 1000,
	}
	if tok != nil {
		ev.InputTokens = max(tok.Input.int64(), 0)
		ev.OutputTokens = max(tok.Output.int64()+tok.Reasoning.int64(), 0)
		ev.CacheReadTokens = max(tok.Cache.Read.int64(), 0)
		ev.CacheCreationTokens = max(tok.Cache.Write.int64(), 0)
	}
	return ev
}

// finishReason maps OpenCode's finish and error fields onto Crush's.
func finishReason(t turn, hasContent bool) message.FinishReason {
	switch t.errType {
	case "aborted", "MessageAbortedError":
		return message.FinishReasonCanceled
	}
	switch t.finish {
	case "stop":
		return message.FinishReasonEndTurn
	case "length":
		return message.FinishReasonMaxTokens
	case "tool-calls", "tool_calls":
		return message.FinishReasonToolUse
	case "content-filter", "content_filter":
		return message.FinishReasonContentFilter
	case "error":
		return message.FinishReasonError
	case "unknown":
		return message.FinishReasonUnknown
	case "":
		if t.errMsg != "" || t.errType != "" {
			return message.FinishReasonError
		}
		if hasContent {
			return message.FinishReasonEndTurn
		}
		return message.FinishReasonUnknown
	default:
		return message.FinishReasonUnknown
	}
}

// sessionTodos derives the session todo list from OpenCode's todo table or,
// failing that, the last todowrite call.
func sessionTodos(rows []Todo, lastInput json.RawMessage) []session.Todo {
	var todos []session.Todo
	if len(rows) > 0 {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Position < rows[j].Position })
		for _, r := range rows {
			status, ok := todoStatus(r.Status)
			if !ok {
				continue
			}
			todos = append(todos, session.Todo{Content: r.Content, Status: status})
		}
		return todos
	}
	if len(lastInput) == 0 {
		return nil
	}
	var in struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(lastInput, &in); err != nil {
		return nil
	}
	for _, item := range in.Todos {
		status, ok := todoStatus(item.Status)
		if !ok {
			continue
		}
		todos = append(todos, session.Todo{Content: item.Content, Status: status})
	}
	return todos
}

func todoStatus(s string) (session.TodoStatus, bool) {
	switch s {
	case "pending":
		return session.TodoStatusPending, true
	case "in_progress":
		return session.TodoStatusInProgress, true
	case "completed":
		return session.TodoStatusCompleted, true
	default:
		return "", false
	}
}

func nonEmpty(in []string) []string {
	out := in[:0:0]
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:n-1])) + "…"
}
