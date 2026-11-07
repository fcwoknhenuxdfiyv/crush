package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// crushToolName maps OpenCode tool names to the Crush tool that renders the
// same kind of call. Tools without an entry keep their name and input.
var crushToolName = map[string]string{
	"bash":      "bash",
	"shell":     "bash",
	"read":      "view",
	"edit":      "edit",
	"write":     "write",
	"glob":      "glob",
	"grep":      "grep",
	"list":      "ls",
	"webfetch":  "fetch",
	"todowrite": "todos",
	"task":      "agent",
	"subagent":  "agent",
}

// translateTool converts an OpenCode tool call into the name and input
// Crush's renderers expect. The input is returned as a JSON string; when
// the stored input is not a JSON object it is wrapped so the generic
// renderer still has something to show.
func translateTool(name string, input json.RawMessage) (string, string) {
	params := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &params); err != nil || params == nil {
			params = map[string]any{}
			var str string
			if json.Unmarshal(input, &str) == nil {
				params["input"] = str
			} else {
				params["input"] = string(input)
			}
		}
	}

	target, known := crushToolName[name]
	if !known {
		return name, encodeParams(params)
	}

	out := map[string]any{}
	switch target {
	case "bash":
		copyKey(out, params, "command", "command")
		copyKey(out, params, "description", "description")
		copyKey(out, params, "workdir", "working_dir")
		copyKey(out, params, "background", "run_in_background")
	case "view":
		copyPath(out, params)
		if offset, ok := params["offset"]; ok {
			if n, ok := asNumber(offset); ok {
				out["offset"] = max(n-1, 0)
			}
		}
		copyKey(out, params, "limit", "limit")
	case "edit":
		copyPath(out, params)
		copyKey(out, params, "oldString", "old_string")
		copyKey(out, params, "newString", "new_string")
		copyKey(out, params, "replaceAll", "replace_all")
	case "write":
		copyPath(out, params)
		copyKey(out, params, "content", "content")
	case "glob":
		copyKey(out, params, "pattern", "pattern")
		copyKey(out, params, "path", "path")
	case "grep":
		copyKey(out, params, "pattern", "pattern")
		copyKey(out, params, "path", "path")
		copyKey(out, params, "include", "include")
	case "ls":
		copyKey(out, params, "path", "path")
		copyKey(out, params, "ignore", "ignore")
	case "fetch":
		copyKey(out, params, "url", "url")
		copyKey(out, params, "format", "format")
		copyKey(out, params, "timeout", "timeout")
	case "todos":
		out["todos"] = translateTodos(params["todos"])
	case "agent":
		copyKey(out, params, "prompt", "prompt")
		copyKey(out, params, "description", "description")
		copyKey(out, params, "subagent_type", "subagent_type")
		copyKey(out, params, "agent", "subagent_type")
	}
	return target, encodeParams(out)
}

func encodeParams(params map[string]any) string {
	data, err := json.Marshal(params)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func copyKey(dst, src map[string]any, from, to string) {
	if v, ok := src[from]; ok && v != nil {
		if _, taken := dst[to]; !taken {
			dst[to] = v
		}
	}
}

// copyPath handles the filePath/path rename OpenCode went through.
func copyPath(dst, src map[string]any) {
	copyKey(dst, src, "filePath", "file_path")
	copyKey(dst, src, "path", "file_path")
}

func asNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

// translateTodos converts todowrite items into Crush todo items.
func translateTodos(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content, _ := m["content"].(string)
		status, _ := m["status"].(string)
		out = append(out, map[string]any{
			"content":     content,
			"status":      translateTodoStatus(status),
			"active_form": "",
		})
	}
	return out
}

func translateTodoStatus(status string) string {
	switch status {
	case "in_progress", "pending", "completed":
		return status
	case "cancelled", "canceled":
		return "completed"
	default:
		return "pending"
	}
}

// childSessionID extracts the sub-agent session an OpenCode task call ran
// in, if the call recorded one.
func childSessionID(item toolItem) string {
	if len(item.metadata) > 0 {
		var meta struct {
			SessionID  string `json:"sessionId"`
			SessionID2 string `json:"sessionID"`
		}
		if json.Unmarshal(item.metadata, &meta) == nil {
			if meta.SessionID != "" {
				return meta.SessionID
			}
			if meta.SessionID2 != "" {
				return meta.SessionID2
			}
		}
	}
	if len(item.input) > 0 {
		var in struct {
			TaskID    string `json:"task_id"`
			SessionID string `json:"sessionID"`
		}
		if json.Unmarshal(item.input, &in) == nil {
			if in.TaskID != "" {
				return in.TaskID
			}
			return in.SessionID
		}
	}
	return ""
}

// toolResultText renders a tool outcome as the text Crush stores. File
// items are described inline unless describeFiles is false, which callers
// use when the single file is carried as image data instead.
func toolResultText(item toolItem, describeFiles bool) (content string, isError bool) {
	switch item.status {
	case "completed":
		var b strings.Builder
		b.WriteString(item.output)
		if describeFiles {
			for _, f := range item.files {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(describeFile(f))
			}
		}
		return b.String(), false
	case "error":
		msg := ""
		if item.err != nil {
			msg = item.err.Message
		}
		if msg == "" {
			msg = item.output
		}
		if msg == "" {
			msg = "Tool execution failed"
		}
		return msg, true
	default:
		return "[Tool execution was interrupted]", true
	}
}

func describeFile(f toolFile) string {
	name := f.Name
	if name == "" {
		name = "file"
	}
	uri := f.URI
	if strings.HasPrefix(uri, "data:") {
		uri = "inline data"
	}
	if f.Mime != "" {
		return fmt.Sprintf("[file: %s (%s) %s]", name, f.Mime, uri)
	}
	return fmt.Sprintf("[file: %s %s]", name, uri)
}
