package opencode

import (
	"encoding/json"
	"strconv"
	"strings"
)

// num decodes a JSON number that may be written as an integer, a float, a
// quoted string, or be null. OpenCode's TypeScript writes whatever the
// runtime had, so the importer is forgiving.
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err == nil {
		*n = num(f)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
	if err != nil {
		*n = 0
		return nil
	}
	*n = num(f)
	return nil
}

func (n num) int64() int64     { return int64(n) }
func (n num) float64() float64 { return float64(n) }

// flag decodes a JSON boolean that may also be written as 0/1 or null.
type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	switch strings.TrimSpace(string(b)) {
	case "true", "1":
		*f = true
	case "false", "0", "null", "":
		*f = false
	default:
		var v bool
		if err := json.Unmarshal(b, &v); err != nil {
			*f = false
			return nil
		}
		*f = flag(v)
	}
	return nil
}

// toolError decodes a tool error that OpenCode has stored either as a plain
// string (legacy) or as a structured {type, message} object.
type toolError struct {
	Type    string
	Message string
}

func (e *toolError) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		e.Message = str
		return nil
	}
	var obj struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		e.Message = s
		return nil
	}
	e.Type = obj.Type
	if e.Type == "" {
		e.Type = obj.Name
	}
	e.Message = obj.Message
	if e.Message == "" {
		e.Message = obj.Data.Message
	}
	return nil
}

// tokens is the token usage OpenCode records on assistant messages, steps
// and compactions.
type tokens struct {
	Input     num `json:"input"`
	Output    num `json:"output"`
	Reasoning num `json:"reasoning"`
	Total     num `json:"total"`
	Cache     struct {
		Read  num `json:"read"`
		Write num `json:"write"`
	} `json:"cache"`
}

// any reports whether any counter is set.
func (t *tokens) any() bool {
	if t == nil {
		return false
	}
	return t.Input > 0 || t.Output > 0 || t.Reasoning > 0 || t.Cache.Read > 0 || t.Cache.Write > 0
}

// modelRef is how OpenCode names a model. Older rows use modelID, newer
// rows use id.
type modelRef struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
	ModelID    string `json:"modelID"`
	Variant    string `json:"variant"`
}

func (m modelRef) model() string {
	if m.ID != "" {
		return m.ID
	}
	return m.ModelID
}

// stamp is the time block OpenCode attaches to messages and content items.
// All values are Unix milliseconds.
type stamp struct {
	Created   num `json:"created"`
	Completed num `json:"completed"`
	Streamed  num `json:"streamed"`
	Ran       num `json:"ran"`
	Start     num `json:"start"`
	End       num `json:"end"`
}

// attachment is a user-supplied file, either inline (base64 Data) or kept in
// the blob table (URI of the form media:<sha256>).
type attachment struct {
	Data        string `json:"data"`
	Mime        string `json:"mime"`
	Name        string `json:"name"`
	URI         string `json:"uri"`
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	Description string `json:"description"`
}

// toolFile is a file item in a tool result.
type toolFile struct {
	URI  string `json:"uri"`
	Mime string `json:"mime"`
	Name string `json:"name"`
}
