package opencode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// PlanOptions selects which sessions an import covers.
type PlanOptions struct {
	// Projects limits the import to sessions whose project directory is one
	// of these paths. Empty means every project.
	Projects []string
	// DataDir, when set, sends every session to this one Crush data
	// directory instead of each project's own.
	DataDir string
	// ResolveDataDir returns the Crush data directory for a project
	// directory. It is consulted when DataDir is empty; when nil the
	// default .crush folder inside the project is used.
	ResolveDataDir func(ctx context.Context, dir string) (string, error)
}

// Plan is the result of matching OpenCode sessions to Crush projects. It
// is computed without reading any messages so that previews stay cheap.
type Plan struct {
	Source     string
	Generation Generation
	Projects   []*ProjectPlan
	Skipped    []SkippedGroup
	Warnings   []string
}

// ProjectPlan lists the sessions headed for one Crush data directory.
type ProjectPlan struct {
	Dir     string
	DataDir string
	// Roots are the top-level sessions, oldest first.
	Roots []Session
	// Children maps an OpenCode session ID to its child sessions, oldest
	// first.
	Children map[string][]Session
	// Total is the number of sessions, roots and descendants included.
	Total int
}

// SkippedGroup is a project directory that was left alone, and why.
type SkippedGroup struct {
	Dir      string `json:"dir"`
	Sessions int    `json:"sessions"`
	Reason   string `json:"reason"`
}

// NewPlan groups the source's sessions by project directory, drops the
// ones whose directory no longer exists and applies the selection options.
func NewPlan(ctx context.Context, src *Source, opts PlanOptions) (*Plan, error) {
	sessions, err := src.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	projects, err := src.Projects(ctx)
	if err != nil {
		return nil, err
	}

	plan := &Plan{Source: src.Path(), Generation: src.Generation()}
	byID := make(map[string]Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}

	directory := func(s Session) string {
		if s.Directory != "" {
			return s.Directory
		}
		if p, ok := projects[s.ProjectID]; ok {
			return p.Worktree
		}
		return ""
	}

	// Every session is filed under the directory of its topmost ancestor
	// so that sub-agent sessions land in the same database as the
	// conversation that spawned them.
	type group struct {
		dir      string
		roots    []Session
		children map[string][]Session
		total    int
	}
	groups := map[string]*group{}
	orphaned := 0
	for _, s := range sessions {
		root := s
		isRoot := true
		for depth := 0; root.ParentID != "" && depth <= len(sessions); depth++ {
			parent, ok := byID[root.ParentID]
			if !ok {
				break
			}
			root = parent
			isRoot = false
		}
		if s.ParentID != "" && isRoot {
			orphaned++
		}
		dir := directory(root)
		g, ok := groups[dir]
		if !ok {
			g = &group{dir: dir, children: map[string][]Session{}}
			groups[dir] = g
		}
		g.total++
		switch {
		case isRoot:
			g.roots = append(g.roots, s)
		default:
			g.children[s.ParentID] = append(g.children[s.ParentID], s)
		}
	}
	if orphaned > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d sessions reference a parent that no longer exists and were imported as top-level sessions", orphaned))
	}

	filters := make([]string, 0, len(opts.Projects))
	for _, p := range opts.Projects {
		filters = append(filters, canonicalPath(p))
	}

	dirs := make([]string, 0, len(groups))
	for dir := range groups {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		g := groups[dir]
		if dir == "" {
			plan.Skipped = append(plan.Skipped, SkippedGroup{Dir: "(none)", Sessions: g.total, Reason: "no project directory recorded"})
			continue
		}
		if len(filters) > 0 && !matchesAny(dir, filters) {
			continue
		}
		info, err := os.Stat(dir)
		switch {
		case err != nil:
			plan.Skipped = append(plan.Skipped, SkippedGroup{Dir: dir, Sessions: g.total, Reason: "directory does not exist"})
			continue
		case !info.IsDir():
			plan.Skipped = append(plan.Skipped, SkippedGroup{Dir: dir, Sessions: g.total, Reason: "not a directory"})
			continue
		}

		dataDir := opts.DataDir
		if dataDir == "" {
			if opts.ResolveDataDir != nil {
				dataDir, err = opts.ResolveDataDir(ctx, dir)
				if err != nil {
					plan.Skipped = append(plan.Skipped, SkippedGroup{Dir: dir, Sessions: g.total, Reason: "resolving Crush data directory: " + err.Error()})
					continue
				}
			} else {
				dataDir = filepath.Join(dir, ".crush")
			}
		}

		sortSessions(g.roots)
		for _, kids := range g.children {
			sortSessions(kids)
		}
		plan.Projects = append(plan.Projects, &ProjectPlan{
			Dir:      dir,
			DataDir:  dataDir,
			Roots:    g.roots,
			Children: g.children,
			Total:    g.total,
		})
	}
	return plan, nil
}

// Sessions is the number of sessions the plan would import.
func (p *Plan) Sessions() int {
	n := 0
	for _, proj := range p.Projects {
		n += proj.Total
	}
	return n
}

func sortSessions(s []Session) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Created != s[j].Created {
			return s[i].Created < s[j].Created
		}
		return s[i].ID < s[j].ID
	})
}

// canonicalPath normalises a directory for comparison, following symlinks
// when the path exists.
func canonicalPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}

func matchesAny(dir string, filters []string) bool {
	c := canonicalPath(dir)
	for _, f := range filters {
		if c == f {
			return true
		}
	}
	return false
}
