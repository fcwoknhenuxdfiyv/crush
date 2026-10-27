package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/projects"
	"github.com/charmbracelet/crush/internal/usage"
	"github.com/charmbracelet/x/exp/charmtone"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

var statsDailyCmd = &cobra.Command{
	Use:   "daily",
	Short: "Show spend per day across all projects",
	Long: `Show token usage and cost per calendar day, summed across every project
Crush knows about.

Each provider call is recorded as it happens, so days are exact from the
version that introduced the ledger onward. Spend recorded before that is
attributed to the day its session was created and marked with "~".`,
	Example: `
# Last 30 days across all known projects
crush stats daily

# Last week, machine readable
crush stats daily --days 7 --json

# Only the project in the current directory
crush stats daily --current
  `,
	RunE: runStatsDaily,
}

func init() {
	statsDailyCmd.Flags().Int("days", 30, "Number of days to include (0 for all time)")
	statsDailyCmd.Flags().Bool("json", false, "Output as JSON")
	statsDailyCmd.Flags().Bool("current", false, "Only include the project in the current directory")
	statsCmd.AddCommand(statsDailyCmd)
}

// dailyProjectResult reports how one project database contributed.
type dailyProjectResult struct {
	Path string `json:"path"`
	// Ledger is false when the database predates per-call tracking; all
	// of its spend is then approximate.
	Ledger bool `json:"ledger"`
	// Skipped is set when the database could not be read.
	Skipped bool   `json:"skipped"`
	Error   string `json:"error,omitempty"`
}

type dailyOutput struct {
	Since    string               `json:"since,omitempty"`
	Days     []usage.Day          `json:"days"`
	Total    usage.Day            `json:"total"`
	Projects []dailyProjectResult `json:"projects"`
}

type dailyTarget struct {
	dbPath     string
	projectDir string
}

func runStatsDaily(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	days, _ := cmd.Flags().GetInt("days")
	jsonOutput, _ := cmd.Flags().GetBool("json")
	currentOnly, _ := cmd.Flags().GetBool("current")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	if days < 0 {
		return fmt.Errorf("--days must be zero or positive")
	}

	var since time.Time
	if days > 0 {
		now := time.Now()
		startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		since = startOfToday.AddDate(0, 0, -(days - 1))
	}

	targets, err := dailyTargets(cmd, dataDir, currentOnly)
	if err != nil {
		return err
	}

	out, err := gatherDailyUsage(ctx, targets, since, time.Local)
	if err != nil {
		return err
	}

	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}

	for _, p := range out.Projects {
		if p.Skipped {
			fmt.Fprintf(cmd.ErrOrStderr(), "warn: skipping %s: %s\n", p.Path, p.Error)
		}
	}

	if len(out.Days) == 0 {
		if days > 0 {
			cmd.Printf("No usage recorded in the last %d days.\n", days)
		} else {
			cmd.Println("No usage recorded.")
		}
		return nil
	}

	if term.IsTerminal(os.Stdout.Fd()) {
		renderDailyTable(os.Stdout, out)
		return nil
	}
	renderDailyPlain(cmd.OutOrStdout(), out)
	return nil
}

// dailyTargets resolves which project databases to read. By default every
// project in projects.json plus the current project's data directory are
// included; --current narrows it to the current project only.
func dailyTargets(cmd *cobra.Command, dataDir string, currentOnly bool) ([]dailyTarget, error) {
	var targets []dailyTarget
	seen := map[string]bool{}
	add := func(dir, projectDir string) {
		dbPath := filepath.Join(dir, "crush.db")
		if abs, err := filepath.Abs(dbPath); err == nil {
			dbPath = abs
		}
		if seen[dbPath] {
			return
		}
		if _, err := os.Stat(dbPath); err != nil {
			return
		}
		seen[dbPath] = true
		targets = append(targets, dailyTarget{dbPath: dbPath, projectDir: projectDir})
	}

	if currentOnly || dataDir != "" {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return nil, err
		}
		if dataDir == "" {
			cfg, err := config.Init(cmd.Context(), cwd, dataDir, false)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize config: %w", err)
			}
			dataDir = cfg.Config().Options.DataDirectory
		}
		add(dataDir, cwd)
		if currentOnly {
			return targets, nil
		}
	}

	projectList, err := projects.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load projects: %w", err)
	}
	for _, p := range projectList.Projects {
		add(p.DataDir, p.Path)
	}
	return targets, nil
}

// gatherDailyUsage reads every target database in parallel and merges the
// per-day results. Unreadable databases are reported, not fatal.
func gatherDailyUsage(ctx context.Context, targets []dailyTarget, since time.Time, loc *time.Location) (dailyOutput, error) {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		groups  [][]usage.Day
		results []dailyProjectResult
	)
	sem := make(chan struct{}, 10)

	for _, target := range targets {
		wg.Add(1)
		go func(target dailyTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result := dailyProjectResult{Path: target.projectDir}
			days, hasLedger, err := readDailyUsage(ctx, target.dbPath, since, loc)
			if err != nil {
				result.Skipped = true
				result.Error = err.Error()
			} else {
				result.Ledger = hasLedger
			}

			mu.Lock()
			defer mu.Unlock()
			results = append(results, result)
			if err == nil {
				groups = append(groups, days)
			}
		}(target)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })

	merged := usage.MergeDays(groups...)
	out := dailyOutput{
		Days:     merged,
		Total:    usage.Total(merged),
		Projects: results,
	}
	if !since.IsZero() {
		out.Since = since.In(loc).Format(usage.DateLayout)
	}
	return out, nil
}

func readDailyUsage(ctx context.Context, dbPath string, since time.Time, loc *time.Location) ([]usage.Day, bool, error) {
	conn, err := db.ConnectReadOnly(ctx, dbPath)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	return usage.DailyFromDB(ctx, conn, since, loc)
}

var dailyHeaders = []string{"Date", "Sessions", "Turns", "Input", "Cache Read", "Cache Write", "Output", "Cost"}

func dailyRow(label string, d usage.Day) []string {
	return []string{
		label,
		fmt.Sprintf("%d", d.Sessions),
		formatDailyTurns(d),
		formatDailyTokens(d.InputTokens, d),
		formatDailyTokens(d.CacheReadTokens, d),
		formatDailyTokens(d.CacheCreationTokens, d),
		formatDailyTokens(d.OutputTokens, d),
		formatDailyCost(d),
	}
}

func renderDailyTable(w io.Writer, out dailyOutput) {
	today := time.Now().In(time.Local).Format(usage.DateLayout)
	totalRow := len(out.Days)

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(charmtone.Malibu).Padding(0, 1)
	cellStyle := lipgloss.NewStyle().Padding(0, 1)
	numberStyle := cellStyle.Align(lipgloss.Right)
	todayStyle := lipgloss.NewStyle().Bold(true).Foreground(charmtone.Guac)
	totalStyle := lipgloss.NewStyle().Bold(true)

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderRow(false).
		Headers(dailyHeaders...).
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
			switch {
			case row == totalRow:
				return style.Inherit(totalStyle)
			case row >= 0 && row < len(out.Days) && out.Days[row].Date == today:
				return style.Inherit(todayStyle)
			}
			return style
		})

	for _, d := range out.Days {
		t.Row(dailyRow(d.Date, d)...)
	}
	t.Row(dailyRow("Total", out.Total)...)

	lipgloss.Fprintln(w, t)
	if out.Total.Approximate {
		note := lipgloss.NewStyle().Foreground(charmtone.Squid).Render(
			"~ includes spend recorded before per-call tracking, attributed to each session's creation day.")
		lipgloss.Fprintln(w, note)
	}
}

func renderDailyPlain(w io.Writer, out dailyOutput) {
	fmt.Fprintln(w, strings.Join(dailyHeaders, "\t"))
	for _, d := range out.Days {
		fmt.Fprintln(w, strings.Join(dailyRow(d.Date, d), "\t"))
	}
	fmt.Fprintln(w, strings.Join(dailyRow("Total", out.Total), "\t"))
}

func formatDailyCost(d usage.Day) string {
	cost := fmt.Sprintf("$%.2f", d.Cost)
	if d.Approximate {
		return "~" + cost
	}
	return cost
}

// formatDailyTurns prints the number of recorded calls. Days whose spend
// is entirely approximate have no call records, so a dash is shown
// instead of a misleading zero.
func formatDailyTurns(d usage.Day) string {
	if d.Turns == 0 && d.Approximate {
		return "-"
	}
	return fmt.Sprintf("%d", d.Turns)
}

func formatDailyTokens(n int64, d usage.Day) string {
	if n == 0 && d.Turns == 0 && d.Approximate {
		return "-"
	}
	return compactTokens(n)
}

// compactTokens renders a token count the way the TUI does: 1.2K, 3.4M.
func compactTokens(n int64) string {
	var s string
	switch {
	case n >= 1_000_000:
		s = fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		s = fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
	s = strings.Replace(s, ".0K", "K", 1)
	s = strings.Replace(s, ".0M", "M", 1)
	return s
}
