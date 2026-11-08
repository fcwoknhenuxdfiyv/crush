package opencode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Grouping decides how sessions are assigned to Crush data directories.
type Grouping string

const (
	// GroupByProject files every session of one OpenCode project (one
	// repository, however many worktrees or clones it was used from) under
	// a single Crush directory, the way OpenCode's own picker lists them.
	// Sessions without a project are grouped by directory.
	GroupByProject Grouping = "project"
	// GroupByDirectory files each session under the directory it was
	// recorded in, matching Crush's per-directory session picker exactly.
	GroupByDirectory Grouping = "directory"
)

// ParseGrouping validates a user-supplied grouping name. The empty string
// selects the default, GroupByProject.
func ParseGrouping(s string) (Grouping, error) {
	switch Grouping(s) {
	case GroupByProject, GroupByDirectory:
		return Grouping(s), nil
	case "":
		return GroupByProject, nil
	}
	return "", fmt.Errorf("unknown grouping %q (want %q or %q)", s, GroupByProject, GroupByDirectory)
}

// globalProjectID is the project OpenCode assigns to directories outside
// any version-controlled repository. It spans unrelated directories, so it
// never acts as a grouping key.
const globalProjectID = "global"

// projectKeyPrefix marks group keys that stand for a whole project rather
// than a single directory.
const projectKeyPrefix = "project:"

// PlanOptions selects which sessions an import covers.
type PlanOptions struct {
	// GroupBy decides how sessions map to Crush directories. The zero
	// value means GroupByProject.
	GroupBy Grouping
	// Projects limits the import to sessions recorded in (or grouped
	// under) one of these directories. Empty means every project.
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
	GroupBy    Grouping
	Projects   []*ProjectPlan
	Skipped    []SkippedGroup
	Warnings   []string
}

// ProjectPlan lists the sessions headed for one Crush data directory.
type ProjectPlan struct {
	// Dir is the directory whose Crush database receives the sessions.
	Dir     string
	DataDir string
	// Folded lists the other directories whose sessions were pulled into
	// Dir because they belong to the same OpenCode project.
	Folded []FoldedDirectory
	// Roots are the top-level sessions, oldest first.
	Roots []Session
	// Children maps an OpenCode session ID to its child sessions, oldest
	// first.
	Children map[string][]Session
	// Total is the number of sessions, roots and descendants included.
	Total int
}

// FoldedDirectory is a directory whose sessions were merged into another
// directory's Crush database.
type FoldedDirectory struct {
	Dir      string `json:"dir"`
	Sessions int    `json:"sessions"`
	// Exists reports whether the directory is still on disk.
	Exists bool `json:"exists"`
}

// SkippedGroup is a project directory that was left alone, and why.
type SkippedGroup struct {
	Dir      string `json:"dir"`
	Sessions int    `json:"sessions"`
	Reason   string `json:"reason"`
}

// group is one candidate Crush destination while the plan is being built.
type group struct {
	// dir is the destination directory, or "" when none is known.
	dir      string
	roots    []Session
	children map[string][]Session
	total    int
	// byDir counts sessions per recorded directory, used to report what
	// was folded together and to pick a fallback destination.
	byDir map[string]int
	// latest is the most recent activity per recorded directory.
	latest map[string]int64
}

func newGroup(dir string) *group {
	return &group{
		dir:      dir,
		children: map[string][]Session{},
		byDir:    map[string]int{},
		latest:   map[string]int64{},
	}
}

func (g *group) add(s Session, recordedDir string, isRoot bool) {
	g.total++
	g.byDir[recordedDir]++
	if s.Updated > g.latest[recordedDir] {
		g.latest[recordedDir] = s.Updated
	}
	if isRoot {
		g.roots = append(g.roots, s)
		return
	}
	g.children[s.ParentID] = append(g.children[s.ParentID], s)
}

// NewPlan groups the source's sessions into Crush destinations, drops the
// ones with nowhere to go and applies the selection options.
func NewPlan(ctx context.Context, src *Source, opts PlanOptions) (*Plan, error) {
	groupBy := opts.GroupBy
	if groupBy == "" {
		groupBy = GroupByProject
	}
	sessions, err := src.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	projects, err := src.Projects(ctx)
	if err != nil {
		return nil, err
	}

	plan := &Plan{Source: src.Path(), Generation: src.Generation(), GroupBy: groupBy}
	byID := make(map[string]Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}

	directory := func(s Session) string {
		if s.Directory != "" {
			return s.Directory
		}
		if p, ok := projects[s.ProjectID]; ok && p.ID != globalProjectID {
			return p.Worktree
		}
		return ""
	}

	// Every session is keyed by its topmost ancestor so that sub-agent
	// sessions land in the same database as the conversation that
	// spawned them.
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
		key := dir
		if groupBy == GroupByProject && root.ProjectID != "" && root.ProjectID != globalProjectID {
			key = projectKeyPrefix + root.ProjectID
		}
		g, ok := groups[key]
		if !ok {
			g = newGroup(dir)
			groups[key] = g
		}
		g.add(s, dir, isRoot)
	}
	if orphaned > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d sessions reference a parent that no longer exists and were imported as top-level sessions", orphaned))
	}

	// Project groups go to the project's worktree; when that is gone they
	// fall back to the most recently used recorded directory that still
	// exists. Directory groups can only go where they were recorded.
	for key, g := range groups {
		projectID, ok := strings.CutPrefix(key, projectKeyPrefix)
		if !ok {
			continue
		}
		g.dir = ""
		if p, ok := projects[projectID]; ok && p.Worktree != "" && isDir(p.Worktree) {
			g.dir = p.Worktree
		} else {
			g.dir = g.fallbackDir()
		}
	}

	filters := make([]string, 0, len(opts.Projects))
	for _, p := range opts.Projects {
		filters = append(filters, canonicalPath(p))
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if groups[keys[i]].dir != groups[keys[j]].dir {
			return groups[keys[i]].dir < groups[keys[j]].dir
		}
		return keys[i] < keys[j]
	})

	for _, key := range keys {
		g := groups[key]
		if len(filters) > 0 && !g.matches(filters) {
			continue
		}
		if g.dir == "" {
			plan.skipMissing(g)
			continue
		}
		info, err := os.Stat(g.dir)
		switch {
		case err != nil:
			plan.skip(g, "directory does not exist")
			continue
		case !info.IsDir():
			plan.skip(g, "not a directory")
			continue
		}

		dataDir := opts.DataDir
		if dataDir == "" {
			if opts.ResolveDataDir != nil {
				dataDir, err = opts.ResolveDataDir(ctx, g.dir)
				if err != nil {
					plan.skip(g, "resolving Crush data directory: "+err.Error())
					continue
				}
			} else {
				dataDir = filepath.Join(g.dir, ".crush")
			}
		}

		sortSessions(g.roots)
		for _, kids := range g.children {
			sortSessions(kids)
		}
		plan.Projects = append(plan.Projects, &ProjectPlan{
			Dir:      g.dir,
			DataDir:  dataDir,
			Folded:   g.folded(),
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

// skip records a group with a known destination that will not be
// imported.
func (p *Plan) skip(g *group, reason string) {
	p.Skipped = append(p.Skipped, SkippedGroup{Dir: g.dir, Sessions: g.total, Reason: reason})
}

// skipMissing records a group that has nowhere to go. It is reported once
// per recorded directory so the user can see every path involved.
func (p *Plan) skipMissing(g *group) {
	for _, dir := range g.sortedDirs() {
		reason := "directory does not exist"
		if dir == "" {
			reason = "no project directory recorded"
		}
		p.Skipped = append(p.Skipped, SkippedGroup{Dir: displayDir(dir), Sessions: g.byDir[dir], Reason: reason})
	}
}

// matches reports whether the group's destination or any directory folded
// into it is one of the filters.
func (g *group) matches(filters []string) bool {
	if g.dir != "" && matchesAny(g.dir, filters) {
		return true
	}
	for dir := range g.byDir {
		if dir != "" && matchesAny(dir, filters) {
			return true
		}
	}
	return false
}

// fallbackDir picks the most recently used recorded directory that still
// exists, or "" when none does.
func (g *group) fallbackDir() string {
	best := ""
	var bestTime int64 = -1
	for dir, t := range g.latest {
		if dir == "" || !isDir(dir) {
			continue
		}
		if t > bestTime || (t == bestTime && dir < best) {
			best, bestTime = dir, t
		}
	}
	return best
}

// folded lists the recorded directories other than the destination.
func (g *group) folded() []FoldedDirectory {
	var out []FoldedDirectory
	for _, dir := range g.sortedDirs() {
		if dir == g.dir {
			continue
		}
		out = append(out, FoldedDirectory{
			Dir:      displayDir(dir),
			Sessions: g.byDir[dir],
			Exists:   dir != "" && isDir(dir),
		})
	}
	return out
}

func (g *group) sortedDirs() []string {
	dirs := make([]string, 0, len(g.byDir))
	for dir := range g.byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

func displayDir(dir string) string {
	if dir == "" {
		return "(none)"
	}
	return dir
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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
