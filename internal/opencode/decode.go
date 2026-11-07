package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// turnKind classifies a layout-neutral message.
type turnKind int

const (
	kindDropped turnKind = iota
	kindUser
	kindHidden
	kindShell
	kindAssistant
	kindCompaction
)

// turn is one OpenCode message after decoding, in a form shared by both
// storage layouts. Times are Unix milliseconds.
type turn struct {
	id      string
	kind    turnKind
	drop    string
	created int64
	updated int64

	// User, hidden and compaction messages.
	text  string
	files []attachment

	// Shell messages.
	command  string
	output   string
	exitCode int

	// Assistant and compaction messages.
	provider  string
	model     string
	completed int64
	reasoning []reasoningItem
	texts     []string
	tools     []toolItem
	finish    string
	errType   string
	errMsg    string
	cost      float64
	hasCost   bool
	tokens    *tokens
	steps     []stepUsage
	summary   bool
	dropped   map[string]int
}

type reasoningItem struct {
	text  string
	start int64
	end   int64
}

// toolItem is one tool call with its recorded outcome.
type toolItem struct {
	id       string
	name     string
	status   string
	input    json.RawMessage
	output   string
	files    []toolFile
	err      *toolError
	metadata json.RawMessage
	start    int64
	end      int64
}

// stepUsage is the usage of one LLM step inside a legacy assistant message.
type stepUsage struct {
	cost    float64
	hasCost bool
	tokens  *tokens
	at      int64
}

func (t *turn) dropPart(name string) {
	if t.dropped == nil {
		t.dropped = map[string]int{}
	}
	t.dropped[name]++
}

// hasUsage reports whether the turn recorded any cost or token counts.
func (t *turn) hasUsage() bool {
	return (t.hasCost && t.cost > 0) || t.tokens.any()
}

func ms(n num) int64 { return n.int64() }

// decode turns a stored message into a turn according to the layout.
func decode(gen Generation, msg Message) (turn, error) {
	if gen == GenerationV1 {
		return decodeV1(msg)
	}
	return decodeV2(msg)
}

// ---- Current layout (session_message) ----

type v2User struct {
	Text  string            `json:"text"`
	Files []json.RawMessage `json:"files"`
	Time  stamp             `json:"time"`
}

type v2Text struct {
	Text string `json:"text"`
	Time stamp  `json:"time"`
}

type v2Shell struct {
	Command string `json:"command"`
	Status  string `json:"status"`
	Exit    *num   `json:"exit"`
	Output  *struct {
		Output string `json:"output"`
	} `json:"output"`
	Time stamp `json:"time"`
}

type v2Assistant struct {
	Model     modelRef          `json:"model"`
	Content   []json.RawMessage `json:"content"`
	Finish    string            `json:"finish"`
	RawFinish string            `json:"rawFinish"`
	Cost      *num              `json:"cost"`
	Tokens    *tokens           `json:"tokens"`
	Error     *toolError        `json:"error"`
	Time      stamp             `json:"time"`
}

type v2Content struct {
	Type     string       `json:"type"`
	Text     string       `json:"text"`
	Time     *stamp       `json:"time"`
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Executed flag         `json:"executed"`
	State    *v2ToolState `json:"state"`
}

type v2ToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"`
	Error    *toolError      `json:"error"`
	Metadata json.RawMessage `json:"metadata"`
}

type v2Compaction struct {
	Status  string    `json:"status"`
	Reason  string    `json:"reason"`
	Summary string    `json:"summary"`
	Model   *modelRef `json:"model"`
	Cost    *num      `json:"cost"`
	Tokens  *tokens   `json:"tokens"`
	Time    stamp     `json:"time"`
}

func decodeV2(msg Message) (turn, error) {
	t := turn{id: msg.ID, created: msg.Created, updated: msg.Updated}
	switch msg.Type {
	case "user":
		var u v2User
		if err := json.Unmarshal(msg.Data, &u); err != nil {
			return t, fmt.Errorf("decode user message %s: %w", msg.ID, err)
		}
		t.kind = kindUser
		t.text = u.Text
		for _, raw := range u.Files {
			var a attachment
			if err := json.Unmarshal(raw, &a); err == nil {
				t.files = append(t.files, a)
			}
		}
	case "synthetic", "system", "skill":
		var s v2Text
		if err := json.Unmarshal(msg.Data, &s); err != nil {
			return t, fmt.Errorf("decode %s message %s: %w", msg.Type, msg.ID, err)
		}
		t.kind = kindHidden
		t.text = s.Text
	case "shell":
		var sh v2Shell
		if err := json.Unmarshal(msg.Data, &sh); err != nil {
			return t, fmt.Errorf("decode shell message %s: %w", msg.ID, err)
		}
		t.kind = kindShell
		t.command = sh.Command
		if sh.Output != nil {
			t.output = sh.Output.Output
		}
		if sh.Exit != nil {
			t.exitCode = int(sh.Exit.int64())
		}
	case "assistant":
		var a v2Assistant
		if err := json.Unmarshal(msg.Data, &a); err != nil {
			return t, fmt.Errorf("decode assistant message %s: %w", msg.ID, err)
		}
		t.kind = kindAssistant
		t.provider = a.Model.ProviderID
		t.model = a.Model.model()
		t.completed = ms(a.Time.Completed)
		if a.Time.Created > 0 {
			t.created = ms(a.Time.Created)
		}
		t.finish = a.Finish
		if a.Error != nil {
			t.errType = a.Error.Type
			t.errMsg = a.Error.Message
		}
		if a.Cost != nil {
			t.cost = a.Cost.float64()
			t.hasCost = true
		}
		t.tokens = a.Tokens
		// Content split out into session_part comes first, then whatever
		// is still inline, matching how OpenCode reads its own rows.
		items := make([]json.RawMessage, 0, len(msg.Parts)+len(a.Content))
		for _, p := range msg.Parts {
			items = append(items, p.Data)
		}
		items = append(items, a.Content...)
		for _, raw := range items {
			var c v2Content
			if err := json.Unmarshal(raw, &c); err != nil {
				t.dropPart("undecodable-content")
				continue
			}
			switch c.Type {
			case "text":
				t.texts = append(t.texts, c.Text)
			case "reasoning":
				item := reasoningItem{text: c.Text}
				if c.Time != nil {
					item.start = ms(c.Time.Created)
					item.end = ms(c.Time.Completed)
				}
				t.reasoning = append(t.reasoning, item)
			case "tool":
				item := toolItem{id: c.ID, name: c.Name}
				if c.Time != nil {
					item.start = ms(c.Time.Created)
					item.end = ms(c.Time.Completed)
				}
				if c.State != nil {
					item.status = c.State.Status
					item.input = c.State.Input
					item.err = c.State.Error
					item.metadata = c.State.Metadata
					item.output, item.files = decodeToolContent(c.State.Content)
				}
				t.tools = append(t.tools, item)
			default:
				t.dropPart("content:" + c.Type)
			}
		}
	case "compaction":
		var c v2Compaction
		if err := json.Unmarshal(msg.Data, &c); err != nil {
			return t, fmt.Errorf("decode compaction message %s: %w", msg.ID, err)
		}
		if c.Status != "completed" {
			t.kind = kindDropped
			t.drop = "compaction:" + c.Status
			return t, nil
		}
		t.kind = kindCompaction
		t.text = c.Summary
		t.completed = ms(c.Time.Completed)
		if c.Model != nil {
			t.provider = c.Model.ProviderID
			t.model = c.Model.model()
		}
		if c.Cost != nil {
			t.cost = c.Cost.float64()
			t.hasCost = true
		}
		t.tokens = c.Tokens
	case "idle", "agent-switched", "model-switched", "location-switched":
		t.kind = kindDropped
		t.drop = msg.Type
	default:
		t.kind = kindDropped
		t.drop = "unknown:" + msg.Type
	}
	return t, nil
}

// decodeToolContent flattens a tool result's content list into text plus
// any file items. Legacy rows store a plain string instead of a list.
func decodeToolContent(raw json.RawMessage) (string, []toolFile) {
	if len(raw) == 0 {
		return "", nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str, nil
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		URI  string `json:"uri"`
		Mime string `json:"mime"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return string(raw), nil
	}
	var (
		texts []string
		files []toolFile
	)
	for _, item := range items {
		switch item.Type {
		case "file":
			files = append(files, toolFile{URI: item.URI, Mime: item.Mime, Name: item.Name})
		default:
			if item.Text != "" {
				texts = append(texts, item.Text)
			}
		}
	}
	return strings.Join(texts, "\n"), files
}

// ---- Legacy layout (message + part) ----

type v1Message struct {
	Role       string     `json:"role"`
	Time       stamp      `json:"time"`
	Model      *modelRef  `json:"model"`
	ModelID    string     `json:"modelID"`
	ProviderID string     `json:"providerID"`
	Cost       *num       `json:"cost"`
	Tokens     *tokens    `json:"tokens"`
	Summary    flag       `json:"summary"`
	Error      *toolError `json:"error"`
	Finish     string     `json:"finish"`
}

type v1Part struct {
	Type      string   `json:"type"`
	Text      string   `json:"text"`
	Synthetic flag     `json:"synthetic"`
	Time      *stamp   `json:"time"`
	Tool      string   `json:"tool"`
	CallID    string   `json:"callID"`
	State     *v1State `json:"state"`
	Reason    string   `json:"reason"`
	Tokens    *tokens  `json:"tokens"`
	Cost      *num     `json:"cost"`
	Mime      string   `json:"mime"`
	Filename  string   `json:"filename"`
	URL       string   `json:"url"`
}

type v1State struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Output   json.RawMessage `json:"output"`
	Error    *toolError      `json:"error"`
	Metadata json.RawMessage `json:"metadata"`
	Time     *stamp          `json:"time"`
}

func decodeV1(msg Message) (turn, error) {
	t := turn{id: msg.ID, created: msg.Created, updated: msg.Updated}
	var env v1Message
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return t, fmt.Errorf("decode message %s: %w", msg.ID, err)
	}
	if env.Time.Created > 0 {
		t.created = ms(env.Time.Created)
	}
	switch env.Role {
	case "user":
		t.kind = kindUser
		var (
			texts    []string
			allSynth = true
		)
		for _, p := range msg.Parts {
			var part v1Part
			if err := json.Unmarshal(p.Data, &part); err != nil {
				t.dropPart("undecodable-part")
				continue
			}
			switch part.Type {
			case "text":
				texts = append(texts, part.Text)
				if !bool(part.Synthetic) {
					allSynth = false
				}
			case "file":
				t.files = append(t.files, attachment{Mime: part.Mime, Name: part.Filename, URL: part.URL})
			default:
				t.dropPart(part.Type)
			}
		}
		t.text = strings.Join(texts, "\n\n")
		if len(texts) > 0 && allSynth {
			t.kind = kindHidden
		}
		if len(texts) == 0 && len(t.files) == 0 {
			t.kind = kindDropped
			t.drop = "empty-user"
		}
	case "assistant":
		t.kind = kindAssistant
		t.providerID(env)
		t.completed = ms(env.Time.Completed)
		t.finish = env.Finish
		t.summary = bool(env.Summary)
		if env.Error != nil {
			t.errType = env.Error.Type
			t.errMsg = env.Error.Message
		}
		if env.Cost != nil {
			t.cost = env.Cost.float64()
			t.hasCost = true
		}
		t.tokens = env.Tokens
		for _, p := range msg.Parts {
			var part v1Part
			if err := json.Unmarshal(p.Data, &part); err != nil {
				t.dropPart("undecodable-part")
				continue
			}
			switch part.Type {
			case "text":
				t.texts = append(t.texts, part.Text)
			case "reasoning":
				item := reasoningItem{text: part.Text}
				if part.Time != nil {
					item.start = ms(part.Time.Start)
					item.end = ms(part.Time.End)
				}
				t.reasoning = append(t.reasoning, item)
			case "tool":
				item := toolItem{id: part.CallID, name: part.Tool}
				if part.State != nil {
					item.status = part.State.Status
					item.input = part.State.Input
					item.err = part.State.Error
					item.metadata = part.State.Metadata
					item.output, item.files = decodeToolContent(part.State.Output)
					if part.State.Time != nil {
						item.start = ms(part.State.Time.Start)
						item.end = ms(part.State.Time.End)
					}
				}
				t.tools = append(t.tools, item)
			case "step-finish":
				step := stepUsage{tokens: part.Tokens, at: p.Created}
				if part.Cost != nil {
					step.cost = part.Cost.float64()
					step.hasCost = true
				}
				if t.finish == "" && part.Reason != "" {
					t.finish = part.Reason
				}
				t.steps = append(t.steps, step)
			default:
				t.dropPart(part.Type)
			}
		}
	default:
		t.kind = kindDropped
		t.drop = "unknown:" + env.Role
	}
	return t, nil
}

func (t *turn) providerID(env v1Message) {
	t.provider = env.ProviderID
	t.model = env.ModelID
	if env.Model != nil {
		if t.provider == "" {
			t.provider = env.Model.ProviderID
		}
		if t.model == "" {
			t.model = env.Model.model()
		}
	}
}
