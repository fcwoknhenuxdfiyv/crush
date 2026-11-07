// Package opencode reads session history out of an OpenCode database and
// converts it into Crush sessions, messages and usage events.
//
// OpenCode has stored its history in two schemas. The legacy layout keeps
// one row per message in a message table and the message's content in a
// separate part table. The current layout stores each message, content
// included, as a single JSON document in session_message, optionally
// spilling large assistant content into session_part. Both are supported;
// the current layout wins when both are present because it is a superset.
//
// The source database is only ever read.
package opencode

import "encoding/json"

// Generation identifies which OpenCode storage layout a database uses.
type Generation int

const (
	// GenerationUnknown means no recognised OpenCode tables were found.
	GenerationUnknown Generation = iota
	// GenerationV1 is the legacy session/message/part layout.
	GenerationV1
	// GenerationV2 is the session_v2/session_message layout.
	GenerationV2
)

// String implements fmt.Stringer.
func (g Generation) String() string {
	switch g {
	case GenerationV1:
		return "v1"
	case GenerationV2:
		return "v2"
	default:
		return "unknown"
	}
}

// Session is a session row in either layout. Times are Unix milliseconds,
// as stored by OpenCode.
type Session struct {
	ID        string
	ParentID  string
	ProjectID string
	Directory string
	Slug      string
	Title     string
	Cost      float64
	Created   int64
	Updated   int64
}

// Project is a row of the project table. Worktree is the directory a
// session falls back to when its own directory is empty.
type Project struct {
	ID       string
	Worktree string
	Name     string
}

// Message is one message row with its content still encoded. In the
// current layout Data is the whole message document and Parts holds any
// session_part rows (by ordinal). In the legacy layout Data is the message
// envelope and Parts holds the part rows (by id), which carry the content.
type Message struct {
	ID      string
	Seq     int64
	Type    string
	Created int64
	Updated int64
	Data    json.RawMessage
	Parts   []RawPart
}

// RawPart is a content fragment stored outside the message document.
type RawPart struct {
	ID      string
	Ordinal int64
	Created int64
	Updated int64
	Data    json.RawMessage
}

// Todo is a row of the todo table.
type Todo struct {
	Content  string
	Status   string
	Priority string
	Position int64
}

// Blob is a media attachment stored by content hash.
type Blob struct {
	Hash string
	Mime string
	Data []byte
}
