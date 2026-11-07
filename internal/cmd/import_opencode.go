package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/opencode"
	"github.com/charmbracelet/crush/internal/projects"
	"github.com/charmbracelet/x/exp/charmtone"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

var importOpenCodeCmd = &cobra.Command{
	Use:   "opencode",
	Short: "Import OpenCode session history",
	Long: `Import sessions from an OpenCode database into Crush.

Each OpenCode session is filed under the project directory it was recorded
in and written to that project's Crush database, so it appears in the
session picker when Crush runs there. Sub-agent sessions are nested under
the task that spawned them. Sessions whose project directory no longer
exists are skipped with a warning.

Per-call usage is carried over as well, so spend reports stay exact for
the imported history. Pass --no-usage to leave the ledger untouched.

Sessions that already exist in Crush are left alone, so the command is
safe to run again. The OpenCode database is only ever read.`,
	Example: `
# Preview what would be imported from the default OpenCode database
crush import opencode --dry-run

# Import everything
crush import opencode

# Import only sessions recorded in the current project
crush import opencode --current

# Import one project from a copied database
crush import opencode --source ~/backup/opencode.db --project ~/src/app
  `,
	RunE: runImportOpenCode,
}

func init() {
	importOpenCodeCmd.Flags().String("source", "", "Path to the OpenCode database (default: the OpenCode data directory)")
	importOpenCodeCmd.Flags().StringSlice("project", nil, "Only import sessions recorded in this directory (repeatable)")
	importOpenCodeCmd.Flags().Bool("current", false, "Only import sessions recorded in the current directory")
	importOpenCodeCmd.Flags().Bool("dry-run", false, "Show what would be imported without writing anything")
	importOpenCodeCmd.Flags().Bool("json", false, "Output as JSON")
	importOpenCodeCmd.Flags().Bool("no-usage", false, "Do not record per-call usage for imported sessions")
}

// importOpenCodeOutput is the machine-readable report of one run.
type importOpenCodeOutput struct {
	Source     string                    `json:"source"`
	Generation string                    `json:"generation"`
	DryRun     bool                      `json:"dry_run"`
	Projects   []opencode.ProjectResult  `json:"projects"`
	Skipped    []opencode.SkippedGroup   `json:"skipped,omitempty"`
	Warnings   []string                  `json:"warnings,omitempty"`
	Total      importOpenCodeTotals      `json:"total"`
	Errors     []importOpenCodeProjError `json:"errors,omitempty"`
}

type importOpenCodeTotals struct {
	Projects       int `json:"projects"`
	Sessions       int `json:"sessions"`
	Imported       int `json:"imported"`
	AlreadyPresent int `json:"already_present"`
	Failed         int `json:"failed"`
	Skipped        int `json:"skipped"`
	Messages       int `json:"messages"`
	Events         int `json:"usage_events"`
}

type importOpenCodeProjError struct {
	Dir   string `json:"dir"`
	Error string `json:"error"`
}

func runImportOpenCode(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	event.SetNonInteractive(true)

	source, _ := cmd.Flags().GetString("source")
	projectFilters, _ := cmd.Flags().GetStringSlice("project")
	currentOnly, _ := cmd.Flags().GetBool("current")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	jsonOutput, _ := cmd.Flags().GetBool("json")
	noUsage, _ := cmd.Flags().GetBool("no-usage")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	sourcePath, err := resolveOpenCodeSource(source)
	if err != nil {
		return err
	}

	if currentOnly {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		projectFilters = append(projectFilters, cwd)
	}

	src, err := opencode.Open(ctx, sourcePath)
	if err != nil {
		return err
	}
	defer src.Close()

	plan, err := opencode.NewPlan(ctx, src, opencode.PlanOptions{
		Projects:       projectFilters,
		DataDir:        dataDir,
		ResolveDataDir: resolveProjectDataDir,
	})
	if err != nil {
		return err
	}

	out := importOpenCodeOutput{
		Source:     plan.Source,
		Generation: plan.Generation.String(),
		DryRun:     dryRun,
		Skipped:    plan.Skipped,
		Warnings:   plan.Warnings,
	}
	for _, s := range plan.Skipped {
		out.Total.Skipped += s.Sessions
	}

	importer := opencode.NewImporter(src, opencode.ImportOptions{Usage: !noUsage, DryRun: dryRun})
	for _, proj := range plan.Projects {
		res, err := importOpenCodeProject(ctx, importer, proj, dryRun, dataDir == "")
		if err != nil {
			out.Errors = append(out.Errors, importOpenCodeProjError{Dir: proj.Dir, Error: err.Error()})
			continue
		}
		out.Projects = append(out.Projects, *res)
		out.Total.Projects++
		out.Total.Sessions += res.Sessions
		out.Total.Imported += res.Imported
		out.Total.AlreadyPresent += res.AlreadyPresent
		out.Total.Failed += res.Failed
		out.Total.Messages += res.Messages
		out.Total.Events += res.Events
	}

	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}

	for _, w := range out.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warn: %s\n", w)
	}
	for _, s := range out.Skipped {
		fmt.Fprintf(cmd.ErrOrStderr(), "warn: skipping %d session(s) in %s: %s\n", s.Sessions, s.Dir, s.Reason)
	}
	for _, p := range out.Projects {
		for _, w := range p.Warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warn: %s: %s\n", p.Dir, w)
		}
	}
	for _, e := range out.Errors {
		fmt.Fprintf(cmd.ErrOrStderr(), "error: %s: %s\n", e.Dir, e.Error)
	}

	if len(out.Projects) == 0 {
		if out.Total.Skipped > 0 {
			cmd.Println("Nothing to import: every matching session belongs to a directory that no longer exists.")
		} else {
			cmd.Println("Nothing to import.")
		}
		return nil
	}

	if term.IsTerminal(os.Stdout.Fd()) {
		renderImportTable(os.Stdout, out)
	} else {
		renderImportPlain(cmd.OutOrStdout(), out)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("%d project(s) could not be imported", len(out.Errors))
	}
	return nil
}

// resolveOpenCodeSource finds the OpenCode database to read. An explicit
// path may name the file or the directory holding it.
func resolveOpenCodeSource(source string) (string, error) {
	if source == "" {
		source = defaultOpenCodeDatabase()
		if _, err := os.Stat(source); err != nil {
			return "", fmt.Errorf("no OpenCode database found at %s; pass --source to point at one", source)
		}
		return source, nil
	}
	if strings.HasPrefix(source, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			source = filepath.Join(home, source[2:])
		}
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("OpenCode database %s: %w", source, err)
	}
	if info.IsDir() {
		source = filepath.Join(source, "opencode.db")
		if _, err := os.Stat(source); err != nil {
			return "", fmt.Errorf("OpenCode database %s: %w", source, err)
		}
	}
	return source, nil
}

// defaultOpenCodeDatabase is where OpenCode keeps its database by default.
func defaultOpenCodeDatabase() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode", "opencode.db")
}

// resolveProjectDataDir works out where Crush keeps the database for a
// project directory, honouring any data_directory the project's own
// configuration sets, without creating anything.
func resolveProjectDataDir(ctx context.Context, dir string) (string, error) {
	cfg, err := config.Init(ctx, dir, "", false)
	if err != nil {
		return "", err
	}
	return cfg.Config().Options.DataDirectory, nil
}

// importOpenCodeProject imports one project, opening (and migrating) its
// Crush database unless this is a dry run. The project is registered with
// Crush afterwards so it appears in cross-project reports.
func importOpenCodeProject(ctx context.Context, importer *opencode.Importer, proj *opencode.ProjectPlan, dryRun, register bool) (*opencode.ProjectResult, error) {
	if dryRun {
		dbPath := filepath.Join(proj.DataDir, "crush.db")
		if _, err := os.Stat(dbPath); err != nil {
			return importer.ImportProject(ctx, proj, nil)
		}
		conn, err := db.ConnectReadOnly(ctx, dbPath)
		if err != nil {
			return importer.ImportProject(ctx, proj, nil)
		}
		defer conn.Close()
		return importer.ImportProject(ctx, proj, conn)
	}

	conn, err := db.Connect(ctx, proj.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open Crush database: %w", err)
	}
	res, err := importer.ImportProject(ctx, proj, conn)
	releaseErr := db.Release(proj.DataDir)
	if err != nil {
		return nil, err
	}
	if releaseErr != nil {
		return nil, fmt.Errorf("close Crush database: %w", releaseErr)
	}
	if register && res.Imported > 0 {
		if err := projects.Register(proj.Dir, proj.DataDir); err != nil {
			res.Warnings = append(res.Warnings, "could not register project with Crush: "+err.Error())
		}
	}
	return res, nil
}

func importRow(label string, p opencode.ProjectResult) []string {
	return []string{
		label,
		fmt.Sprintf("%d", p.Sessions),
		fmt.Sprintf("%d", p.Imported),
		fmt.Sprintf("%d", p.AlreadyPresent),
		fmt.Sprintf("%d", p.Failed),
		fmt.Sprintf("%d", p.Messages),
		fmt.Sprintf("%d", p.Events),
	}
}

func importTotalsRow(t importOpenCodeTotals) []string {
	return []string{
		"Total",
		fmt.Sprintf("%d", t.Sessions),
		fmt.Sprintf("%d", t.Imported),
		fmt.Sprintf("%d", t.AlreadyPresent),
		fmt.Sprintf("%d", t.Failed),
		fmt.Sprintf("%d", t.Messages),
		fmt.Sprintf("%d", t.Events),
	}
}

var importHeaders = []string{"Project", "Sessions", "Imported", "Present", "Failed", "Messages", "Usage"}

func renderImportTable(w io.Writer, out importOpenCodeOutput) {
	home, _ := os.UserHomeDir()
	totalRow := len(out.Projects)

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(charmtone.Malibu).Padding(0, 1)
	cellStyle := lipgloss.NewStyle().Padding(0, 1)
	numberStyle := cellStyle.Align(lipgloss.Right)
	totalStyle := lipgloss.NewStyle().Bold(true)

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderRow(false).
		Headers(importHeaders...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				if col == 0 {
					return headerStyle
				}
				return headerStyle.Align(lipgloss.Right)
			}
			style := cellStyle
			if col > 0 {
				style = numberStyle
			}
			if row == totalRow {
				return style.Inherit(totalStyle)
			}
			return style
		})

	for _, p := range out.Projects {
		t.Row(importRow(shortenHome(p.Dir, home), p)...)
	}
	t.Row(importTotalsRow(out.Total)...)

	lipgloss.Fprintln(w, t)
	note := importSummaryLine(out)
	if note != "" {
		lipgloss.Fprintln(w, lipgloss.NewStyle().Foreground(charmtone.Squid).Render(note))
	}
}

func renderImportPlain(w io.Writer, out importOpenCodeOutput) {
	fmt.Fprintln(w, strings.Join(importHeaders, "\t"))
	for _, p := range out.Projects {
		fmt.Fprintln(w, strings.Join(importRow(p.Dir, p), "\t"))
	}
	fmt.Fprintln(w, strings.Join(importTotalsRow(out.Total), "\t"))
	if note := importSummaryLine(out); note != "" {
		fmt.Fprintln(w, note)
	}
}

func importSummaryLine(out importOpenCodeOutput) string {
	var parts []string
	if out.DryRun {
		parts = append(parts, "Dry run: nothing was written.")
	}
	if out.Total.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d session(s) skipped because their directory no longer exists.", out.Total.Skipped))
	}
	return strings.Join(parts, " ")
}

func shortenHome(path, home string) string {
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rel
	}
	return path
}
