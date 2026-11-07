package opencode

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/usage"
	"github.com/stretchr/testify/require"
)

func decodeParts(t *testing.T, row db.ImportMessageParams) []message.ContentPart {
	t.Helper()
	parts, err := message.UnmarshalParts([]byte(row.Parts))
	require.NoError(t, err)
	return parts
}

func partOf[T message.ContentPart](t *testing.T, parts []message.ContentPart) T {
	t.Helper()
	for _, p := range parts {
		if v, ok := p.(T); ok {
			return v
		}
	}
	var zero T
	require.Failf(t, "missing part", "no %T in %#v", zero, parts)
	return zero
}

func partsOf[T message.ContentPart](parts []message.ContentPart) []T {
	var out []T
	for _, p := range parts {
		if v, ok := p.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func TestMapSessionV2(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	root := seedV2Conversation(f, dir)
	src := f.open()
	sessions, err := src.Sessions(t.Context())
	require.NoError(t, err)

	m, err := MapSession(t.Context(), src, sessions[0], MapOptions{Usage: true})
	require.NoError(t, err)

	// Session row.
	s := m.Session
	require.Equal(t, root, s.ID)
	require.False(t, s.ParentSessionID.Valid)
	require.Equal(t, "Fix the flaky test", s.Title)
	require.Equal(t, int64(1_700_000_000), s.CreatedAt)
	require.Equal(t, int64(1_700_000_900), s.UpdatedAt)
	require.InDelta(t, 1.5, s.Cost, 1e-9, "own assistant costs, not the stale session column")
	require.Equal(t, int64(20+1500), s.PromptTokens, "context of the last assistant step")
	require.Equal(t, int64(30), s.CompletionTokens)
	require.Equal(t, "msg_6", s.SummaryMessageID.String)
	require.Equal(t, int64(len(m.Messages)), s.MessageCount)
	require.Equal(t, []session.Todo{
		{Content: "Reproduce failure", Status: session.TodoStatusCompleted},
		{Content: "Fix it", Status: session.TodoStatusInProgress},
	}, m.Todos)
	require.True(t, s.Todos.Valid)

	// Messages: user, assistant, tool results, assistant, compaction,
	// synthetic (hidden), aborted assistant.
	ids := make([]string, 0, len(m.Messages))
	roles := make([]string, 0, len(m.Messages))
	for _, row := range m.Messages {
		ids = append(ids, row.ID)
		roles = append(roles, row.Role)
		require.Equal(t, root, row.SessionID)
	}
	require.Equal(t, []string{"msg_1", "msg_3", "msg_3/tools", "msg_4", "msg_6", "msg_7", "msg_8"}, ids)
	require.Equal(t, []string{"user", "assistant", "tool", "assistant", "assistant", "user", "assistant"}, roles)

	for i := 1; i < len(m.Messages); i++ {
		require.Greater(t, m.Messages[i].CreatedAt, m.Messages[i-1].CreatedAt, "created_at must strictly increase")
		require.GreaterOrEqual(t, m.Messages[i].UpdatedAt, m.Messages[i].CreatedAt)
	}
	require.Equal(t, int64(1_700_000_000), m.Messages[0].CreatedAt)

	user := decodeParts(t, m.Messages[0])
	require.Equal(t, "Please fix the flaky test", partOf[message.TextContent](t, user).Text)
	bin := partOf[message.BinaryContent](t, user)
	require.Equal(t, "shot.png", bin.Path)
	require.Equal(t, "image/png", bin.MIMEType)
	require.Equal(t, []byte("PNGDATA"), bin.Data)
	require.Equal(t, message.FinishReason("stop"), partOf[message.Finish](t, user).Reason)

	asst := m.Messages[1]
	require.Equal(t, "claude-opus-4.8", asst.Model.String)
	require.Equal(t, "github-copilot", asst.Provider.String)
	require.Equal(t, int64(1_700_000_007), asst.FinishedAt.Int64)
	parts := decodeParts(t, asst)
	reasoning := partOf[message.ReasoningContent](t, parts)
	require.Equal(t, "Need to look at the test first.", reasoning.Thinking)
	require.Equal(t, int64(1_700_000_001), reasoning.StartedAt)
	require.Equal(t, int64(1_700_000_003), reasoning.FinishedAt)
	require.Equal(t, "Let me look.", partOf[message.TextContent](t, parts).Text)
	calls := partsOf[message.ToolCall](parts)
	require.Len(t, calls, 3)
	require.Equal(t, "view", calls[0].Name)
	require.JSONEq(t, `{"file_path":"/repo/x_test.go","offset":9,"limit":5}`, calls[0].Input)
	require.True(t, calls[0].Finished)
	require.Equal(t, "bash", calls[1].Name)
	require.JSONEq(t, `{"command":"go test ./...","description":"Run tests"}`, calls[1].Input)
	require.Equal(t, "agent", calls[2].Name)
	require.JSONEq(t, `{"prompt":"Find the test","description":"Explore","subagent_type":"explore"}`, calls[2].Input)
	finish := partOf[message.Finish](t, parts)
	require.Equal(t, message.FinishReasonToolUse, finish.Reason)
	require.Equal(t, int64(1_700_000_007), finish.Time)

	tools := decodeParts(t, m.Messages[2])
	results := partsOf[message.ToolResult](tools)
	require.Len(t, results, 3)
	require.Equal(t, "call_read", results[0].ToolCallID)
	require.Equal(t, "view", results[0].Name)
	require.Equal(t, "line 10\nline 11", results[0].Content)
	require.False(t, results[0].IsError)
	require.Equal(t, "exit status 1", results[1].Content)
	require.True(t, results[1].IsError)
	require.Equal(t, "Found it in x_test.go", results[2].Content)
	require.Equal(t, message.FinishReason("stop"), partOf[message.Finish](t, tools).Reason)

	summary := m.Messages[4]
	require.Equal(t, int64(1), summary.IsSummaryMessage)
	require.Equal(t, "## Summary\nFixed the test.", partOf[message.TextContent](t, decodeParts(t, summary)).Text)
	require.Equal(t, message.FinishReasonEndTurn, partOf[message.Finish](t, decodeParts(t, summary)).Reason)

	hidden := decodeParts(t, m.Messages[5])
	require.True(t, partOf[message.TextContent](t, hidden).Hidden)

	aborted := decodeParts(t, m.Messages[6])
	abortFinish := partOf[message.Finish](t, aborted)
	require.Equal(t, message.FinishReasonCanceled, abortFinish.Reason)
	require.Equal(t, "Aborted", abortFinish.Message)

	// Child link for the task call.
	require.Equal(t, map[string]string{"ses_child": "msg_3$$call_task"}, m.Children)

	// Dropped bookkeeping.
	require.Equal(t, 1, m.Dropped["agent-switched"])
	require.Equal(t, 1, m.Dropped["idle"])

	// Usage events.
	require.Len(t, m.Events, 2)
	ev := m.Events[0]
	require.Equal(t, root, ev.SessionID)
	require.Equal(t, string(usage.SourceTurn), ev.Source)
	require.Equal(t, "github-copilot", ev.Provider)
	require.Equal(t, "claude-opus-4.8", ev.Model)
	require.Equal(t, int64(100), ev.InputTokens)
	require.Equal(t, int64(60), ev.OutputTokens, "output includes reasoning")
	require.Equal(t, int64(1000), ev.CacheReadTokens)
	require.Equal(t, int64(200), ev.CacheCreationTokens)
	require.InDelta(t, 1.25, ev.Cost, 1e-9)
	require.Equal(t, int64(1_700_000_007), ev.CreatedAt)
	require.InDelta(t, 0.25, m.Events[1].Cost, 1e-9)
}

func TestMapSessionV2WithoutUsage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	seedV2Conversation(f, t.TempDir())
	src := f.open()
	sessions, err := src.Sessions(t.Context())
	require.NoError(t, err)

	m, err := MapSession(t.Context(), src, sessions[0], MapOptions{})
	require.NoError(t, err)
	require.Empty(t, m.Events)
	require.InDelta(t, 1.5, m.Session.Cost, 1e-9)
}

func TestMapSessionChildIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	seedV2Conversation(f, t.TempDir())
	src := f.open()
	sessions, err := src.Sessions(t.Context())
	require.NoError(t, err)

	m, err := MapSession(t.Context(), src, sessions[1], MapOptions{
		SessionID:       "msg_3$$call_task",
		ParentSessionID: "ses_root",
		Usage:           true,
	})
	require.NoError(t, err)
	require.Equal(t, "msg_3$$call_task", m.Session.ID)
	require.Equal(t, "ses_root", m.Session.ParentSessionID.String)
	require.Equal(t, "Explore the repo", m.Session.Title)
	for _, row := range m.Messages {
		require.Equal(t, "msg_3$$call_task", row.SessionID)
	}
	for _, ev := range m.Events {
		require.Equal(t, "msg_3$$call_task", ev.SessionID)
	}
}

func TestMapSessionV1(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v1Schema)
	root := seedV1Conversation(f, t.TempDir())
	src := f.open()
	sessions, err := src.Sessions(t.Context())
	require.NoError(t, err)

	m, err := MapSession(t.Context(), src, sessions[0], MapOptions{Usage: true})
	require.NoError(t, err)

	s := m.Session
	require.Equal(t, root, s.ID)
	require.Equal(t, "Legacy chat", s.Title)
	require.InDelta(t, 0.5, s.Cost, 1e-9)
	require.Equal(t, "msg_l4", s.SummaryMessageID.String)
	require.Equal(t, []session.Todo{{Content: "Ship it", Status: session.TodoStatusPending}}, m.Todos)

	ids := make([]string, 0, len(m.Messages))
	for _, row := range m.Messages {
		ids = append(ids, row.ID)
	}
	require.Equal(t, []string{"msg_l1", "msg_l2", "msg_l2/tools", "msg_l3", "msg_l4", "msg_l5"}, ids)

	user := decodeParts(t, m.Messages[0])
	require.Equal(t, "quoted prompt", partOf[message.TextContent](t, user).Text, "JSON-quoted prompts are unwrapped")

	asst := decodeParts(t, m.Messages[1])
	require.Equal(t, "gpt-5", m.Messages[1].Model.String)
	require.Equal(t, "openai", m.Messages[1].Provider.String)
	reasoning := partOf[message.ReasoningContent](t, asst)
	require.Equal(t, "Thinking about it.", reasoning.Thinking)
	require.Equal(t, int64(1_600_000_002), reasoning.StartedAt)
	require.Equal(t, int64(1_600_000_003), reasoning.FinishedAt)
	require.Equal(t, "Two files.", partOf[message.TextContent](t, asst).Text)
	call := partOf[message.ToolCall](t, asst)
	require.Equal(t, "call_ls", call.ID)
	require.Equal(t, "bash", call.Name)
	require.JSONEq(t, `{"command":"ls","description":"List files"}`, call.Input)
	require.Equal(t, message.FinishReasonEndTurn, partOf[message.Finish](t, asst).Reason)

	result := partOf[message.ToolResult](t, decodeParts(t, m.Messages[2]))
	require.Equal(t, "a.go\nb.go\n", result.Content)
	require.False(t, result.IsError)

	hidden := decodeParts(t, m.Messages[3])
	require.True(t, partOf[message.TextContent](t, hidden).Hidden)

	require.Equal(t, int64(1), m.Messages[4].IsSummaryMessage)
	require.Equal(t, "Summary of the chat.", partOf[message.TextContent](t, decodeParts(t, m.Messages[4])).Text)

	require.Equal(t, message.FinishReasonCanceled, partOf[message.Finish](t, decodeParts(t, m.Messages[5])).Reason)

	// Session tokens come from the last step of the last assistant turn
	// that reported any, which is the summary here.
	require.Equal(t, int64(30), s.PromptTokens)
	require.Equal(t, int64(10), s.CompletionTokens)

	// One usage event per step-finish, plus one for the summary turn.
	require.Len(t, m.Events, 3)
	require.InDelta(t, 0.2, m.Events[0].Cost, 1e-9)
	require.Equal(t, int64(50), m.Events[0].InputTokens)
	require.Equal(t, int64(10), m.Events[0].OutputTokens)
	require.Equal(t, int64(1_600_000_004), m.Events[0].CreatedAt)
	require.InDelta(t, 0.3, m.Events[1].Cost, 1e-9)
	require.Equal(t, string(usage.SourceTurn), m.Events[2].Source)
	require.Equal(t, int64(30), m.Events[2].InputTokens)

	require.Equal(t, 2, m.Dropped["step-start"])
}

func TestMapSessionTitleFallbacks(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	f.project("prj_1", dir)
	f.sessionV2("ses_a", "prj_1", "", dir, "", 0, 1_000, 2_000)
	f.exec(`UPDATE session_v2 SET slug = '' WHERE id = 'ses_a'`)
	f.messageV2("msg_a1", "ses_a", "user", 1, 1_000, obj{
		"text": "First line of a rather long prompt that goes on and on and on and on and on and on and on and on\nsecond line",
		"time": obj{"created": 1_000},
	})
	f.sessionV2("ses_b", "prj_1", "", dir, "", 0, 3_000, 4_000)
	f.sessionV2("ses_c", "prj_1", "", dir, "", 0, 5_000, 6_000)
	f.exec(`UPDATE session_v2 SET slug = '' WHERE id = 'ses_c'`)
	src := f.open()
	sessions, err := src.Sessions(t.Context())
	require.NoError(t, err)

	a, err := MapSession(t.Context(), src, sessions[0], MapOptions{})
	require.NoError(t, err)
	require.Equal(t, "First line of a rather long prompt that goes on and on and on and on and on and…", a.Session.Title)
	require.LessOrEqual(t, len([]rune(a.Session.Title)), maxTitleLen)

	b, err := MapSession(t.Context(), src, sessions[1], MapOptions{})
	require.NoError(t, err)
	require.Equal(t, "slug-ses_b", b.Session.Title)

	c, err := MapSession(t.Context(), src, sessions[2], MapOptions{})
	require.NoError(t, err)
	require.Equal(t, "Imported OpenCode session ses_c", c.Session.Title)
	require.Zero(t, c.Session.MessageCount)
}

func TestMapSessionShellAndTodosFromCall(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	f.project("prj_1", dir)
	f.sessionV2("ses_s", "prj_1", "", dir, "Shell", 0, 1_000, 2_000)
	f.messageV2("msg_s1", "ses_s", "shell", 1, 1_000, obj{
		"shellID": "sh_1", "command": "ls -la", "status": "exited", "exit": 2,
		"output": obj{"output": "total 0", "cursor": 7, "size": 7, "truncated": false},
		"time":   obj{"created": 1_000, "completed": 1_500},
	})
	f.messageV2("msg_s2", "ses_s", "assistant", 2, 2_000, obj{
		"model": obj{"providerID": "anthropic", "id": "claude"},
		"content": []obj{{
			"type": "tool", "id": "call_todo", "name": "todowrite",
			"state": obj{
				"status": "completed",
				"input": obj{"todos": []obj{
					{"content": "A", "status": "completed", "priority": "high"},
					{"content": "B", "status": "in_progress", "priority": "low"},
					{"content": "C", "status": "cancelled", "priority": "low"},
				}},
				"content": []obj{{"type": "text", "text": "ok"}},
			},
			"time": obj{"created": 2_000, "completed": 2_100},
		}},
		"finish": "tool-calls",
		"time":   obj{"created": 2_000, "completed": 2_100},
	})
	src := f.open()
	sessions, err := src.Sessions(t.Context())
	require.NoError(t, err)

	m, err := MapSession(t.Context(), src, sessions[0], MapOptions{})
	require.NoError(t, err)

	shell := partOf[message.ShellCommand](t, decodeParts(t, m.Messages[0]))
	require.Equal(t, "ls -la", shell.Command)
	require.Equal(t, "total 0", shell.Output)
	require.Equal(t, 2, shell.ExitCode)
	require.Equal(t, "user", m.Messages[0].Role)

	call := partOf[message.ToolCall](t, decodeParts(t, m.Messages[1]))
	require.Equal(t, "todos", call.Name)
	var in struct {
		Todos []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"active_form"`
		} `json:"todos"`
	}
	require.NoError(t, json.Unmarshal([]byte(call.Input), &in))
	require.Len(t, in.Todos, 3)
	require.Equal(t, "completed", in.Todos[2].Status, "cancelled todos read as completed in the call")

	require.Equal(t, []session.Todo{
		{Content: "A", Status: session.TodoStatusCompleted},
		{Content: "B", Status: session.TodoStatusInProgress},
	}, m.Todos, "session todos come from the last todowrite when the todo table is empty")
	require.False(t, m.Session.SummaryMessageID.Valid)
	require.Zero(t, m.Session.Cost)
}

func TestMapSessionInlineImageResult(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	f.project("prj_1", dir)
	f.sessionV2("ses_i", "prj_1", "", dir, "Image", 0, 1_000, 2_000)
	f.messageV2("msg_i1", "ses_i", "assistant", 1, 1_000, obj{
		"model": obj{"providerID": "anthropic", "id": "claude"},
		"content": []obj{{
			"type": "tool", "id": "call_img", "name": "read",
			"state": obj{
				"status": "completed",
				"input":  obj{"path": "/tmp/a.png"},
				"content": []obj{
					{"type": "text", "text": "Image read successfully"},
					{"type": "file", "uri": "data:image/png;base64,UE5H", "mime": "image/png"},
				},
			},
			"time": obj{"created": 1_000, "completed": 1_100},
		}, {
			"type": "tool", "id": "call_two", "name": "read",
			"state": obj{
				"status": "completed",
				"input":  obj{"path": "/tmp/b.pdf"},
				"content": []obj{
					{"type": "file", "uri": "data:application/pdf;base64,UERG", "mime": "application/pdf", "name": "b.pdf"},
				},
			},
			"time": obj{"created": 1_200, "completed": 1_300},
		}, {
			"type": "tool", "id": "call_three", "name": "grep",
			"state": obj{"status": "running", "input": obj{"pattern": "x"}},
			"time":  obj{"created": 1_400},
		}},
		"finish": "tool-calls",
		"time":   obj{"created": 1_000, "completed": 1_500},
	})
	src := f.open()
	sessions, err := src.Sessions(t.Context())
	require.NoError(t, err)

	m, err := MapSession(t.Context(), src, sessions[0], MapOptions{})
	require.NoError(t, err)
	results := partsOf[message.ToolResult](decodeParts(t, m.Messages[1]))
	require.Len(t, results, 3)
	require.Equal(t, "UE5H", results[0].Data)
	require.Equal(t, "image/png", results[0].MIMEType)
	require.Equal(t, "Image read successfully", results[0].Content)
	require.Empty(t, results[1].Data)
	require.Equal(t, "text/plain", results[1].MIMEType)
	require.Equal(t, "[file: b.pdf (application/pdf) inline data]", results[1].Content)
	require.True(t, results[2].IsError)
	require.Equal(t, "[Tool execution was interrupted]", results[2].Content)
	require.Equal(t, int64(0)+1, m.Messages[0].CreatedAt)
	require.Equal(t, int64(2), m.Messages[1].CreatedAt, "tool results sort after the assistant turn")
}

func TestFinishReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		turn    turn
		content bool
		want    message.FinishReason
	}{
		{"stop", turn{finish: "stop"}, true, message.FinishReasonEndTurn},
		{"length", turn{finish: "length"}, true, message.FinishReasonMaxTokens},
		{"tool-calls", turn{finish: "tool-calls"}, true, message.FinishReasonToolUse},
		{"content-filter", turn{finish: "content-filter", errType: "provider.content-filter"}, false, message.FinishReasonContentFilter},
		{"error", turn{finish: "error"}, false, message.FinishReasonError},
		{"unknown", turn{finish: "unknown"}, true, message.FinishReasonUnknown},
		{"aborted wins", turn{finish: "tool-calls", errType: "aborted"}, true, message.FinishReasonCanceled},
		{"legacy abort", turn{errType: "MessageAbortedError"}, false, message.FinishReasonCanceled},
		{"provider error without finish", turn{errType: "provider.error", errMsg: "boom"}, false, message.FinishReasonError},
		{"missing with content", turn{}, true, message.FinishReasonEndTurn},
		{"missing without content", turn{}, false, message.FinishReasonUnknown},
		{"unexpected", turn{finish: "other"}, true, message.FinishReasonUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, finishReason(tc.turn, tc.content))
		})
	}
}

func TestTranslateTool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		tool      string
		input     string
		wantName  string
		wantInput string
	}{
		{"bash drops timeout", "bash", `{"command":"ls","timeout":5,"description":"d","workdir":"/w","background":true}`, "bash", `{"command":"ls","description":"d","working_dir":"/w","run_in_background":true}`},
		{"shell is bash", "shell", `{"command":"ls"}`, "bash", `{"command":"ls"}`},
		{"read offset shifts", "read", `{"filePath":"/f","offset":1,"limit":3}`, "view", `{"file_path":"/f","offset":0,"limit":3}`},
		{"read path key", "read", `{"path":"/f","offset":0}`, "view", `{"file_path":"/f","offset":0}`},
		{"edit", "edit", `{"path":"/f","oldString":"a","newString":"b","replaceAll":true}`, "edit", `{"file_path":"/f","old_string":"a","new_string":"b","replace_all":true}`},
		{"write", "write", `{"filePath":"/f","content":"x"}`, "write", `{"file_path":"/f","content":"x"}`},
		{"glob", "glob", `{"pattern":"**/*.go","path":"/p"}`, "glob", `{"pattern":"**/*.go","path":"/p"}`},
		{"grep", "grep", `{"pattern":"x","path":"/p","include":"*.go"}`, "grep", `{"pattern":"x","path":"/p","include":"*.go"}`},
		{"list", "list", `{"path":"/p","ignore":["a"]}`, "ls", `{"path":"/p","ignore":["a"]}`},
		{"webfetch", "webfetch", `{"url":"https://x","format":"text","timeout":3}`, "fetch", `{"url":"https://x","format":"text","timeout":3}`},
		{"task", "task", `{"description":"d","prompt":"p","subagent_type":"explore","task_id":"ses_1"}`, "agent", `{"prompt":"p","description":"d","subagent_type":"explore"}`},
		{"subagent", "subagent", `{"prompt":"p","agent":"explore"}`, "agent", `{"prompt":"p","subagent_type":"explore"}`},
		{"todowrite", "todowrite", `{"todos":[{"content":"a","status":"cancelled"},{"content":"b","status":"weird"}]}`, "todos", `{"todos":[{"content":"a","status":"completed","active_form":""},{"content":"b","status":"pending","active_form":""}]}`},
		{"unknown passes through", "JIRA_search", `{"jql":"x"}`, "JIRA_search", `{"jql":"x"}`},
		{"question passes through", "question", `{"questions":[{"header":"h"}]}`, "question", `{"questions":[{"header":"h"}]}`},
		{"streaming string input", "bash", `"partial"`, "bash", `{}`},
		{"unknown with string input", "mystery", `"partial"`, "mystery", `{"input":"partial"}`},
		{"empty input", "glob", ``, "glob", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name, input := translateTool(tc.tool, json.RawMessage(tc.input))
			require.Equal(t, tc.wantName, name)
			require.JSONEq(t, tc.wantInput, input)
		})
	}
}

func TestChildSessionID(t *testing.T) {
	t.Parallel()
	require.Equal(t, "ses_m", childSessionID(toolItem{metadata: json.RawMessage(`{"sessionId":"ses_m"}`), input: json.RawMessage(`{"task_id":"ses_i"}`)}))
	require.Equal(t, "ses_i", childSessionID(toolItem{metadata: json.RawMessage(`{"truncated":false}`), input: json.RawMessage(`{"task_id":"ses_i"}`)}))
	require.Equal(t, "ses_s", childSessionID(toolItem{input: json.RawMessage(`{"sessionID":"ses_s"}`)}))
	require.Equal(t, "", childSessionID(toolItem{input: json.RawMessage(`{"prompt":"x"}`)}))
}

func TestClock(t *testing.T) {
	t.Parallel()
	c := newClock()
	require.Equal(t, int64(10), c.next(10_000))
	require.Equal(t, int64(11), c.next(10_500))
	require.Equal(t, int64(12), c.next(10_900))
	require.Equal(t, int64(20), c.next(20_000))
	require.Equal(t, int64(21), c.next(5_000), "never goes backwards")
}

func TestUnquote(t *testing.T) {
	t.Parallel()
	require.Equal(t, "hi there", unquote(`"hi there"`))
	require.Equal(t, `plain`, unquote(`plain`))
	require.Equal(t, `"unterminated`, unquote(`"unterminated`))
	require.Equal(t, `"a" and "b"`, unquote(`"a" and "b"`), "only whole-string quoting is undone")
	require.Equal(t, `"`, unquote(`"`))
}

func TestDecodeDataURL(t *testing.T) {
	t.Parallel()
	data, mime, ok := decodeDataURL("data:image/png;base64,UE5H")
	require.True(t, ok)
	require.Equal(t, "image/png", mime)
	require.Equal(t, []byte("PNG"), data)

	data, mime, ok = decodeDataURL("data:text/plain,hello")
	require.True(t, ok)
	require.Equal(t, "text/plain", mime)
	require.Equal(t, []byte("hello"), data)

	_, _, ok = decodeDataURL("https://example.com")
	require.False(t, ok)
}

func TestJSONLeniency(t *testing.T) {
	t.Parallel()
	var n num
	require.NoError(t, json.Unmarshal([]byte(`"12.5"`), &n))
	require.Equal(t, int64(12), n.int64())
	require.NoError(t, json.Unmarshal([]byte(`null`), &n))
	require.Zero(t, n)
	require.NoError(t, json.Unmarshal([]byte(`"nan?"`), &n))
	require.Zero(t, n)

	var f flag
	require.NoError(t, json.Unmarshal([]byte(`1`), &f))
	require.True(t, bool(f))
	require.NoError(t, json.Unmarshal([]byte(`"yes"`), &f))
	require.False(t, bool(f))

	var e toolError
	require.NoError(t, json.Unmarshal([]byte(`"plain failure"`), &e))
	require.Equal(t, "plain failure", e.Message)
	require.NoError(t, json.Unmarshal([]byte(`{"type":"tool.execution","message":"aborted"}`), &e))
	require.Equal(t, "tool.execution", e.Type)
	require.Equal(t, "aborted", e.Message)
	require.NoError(t, json.Unmarshal([]byte(`{"name":"APIError","data":{"message":"nope"}}`), &e))
	require.Equal(t, "APIError", e.Type)
	require.Equal(t, "nope", e.Message)
}
