package cmd

import (
	"context"
	"database/sql"
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

// statsPeriod is one aggregation granularity for the spend reports. The
// daily and monthly commands differ only in how spend is bucketed and
// labelled, so everything else is shared.
type statsPeriod struct {
	// header labels the first table column.
	header string
	// noun names one period in prose ("day", "month").
	noun string
	// layout is the time format of usage.Day.Date for this granularity.
	layout string
	// read aggregates one project database.
	read func(ctx context.Context, conn *sql.DB, since time.Time, loc *time.Location) ([]usage.Day, bool, error)
}

var (
	dailyPeriod = statsPeriod{
		header: "Date",
		noun:   "day",
		layout: usage.DateLayout,
		read:   usage.DailyFromDB,
	}
	monthlyPeriod = statsPeriod{
		header: "Month",
		noun:   "month",
		layout: usage.MonthLayout,
		read:   usage.MonthlyFromDB,
	}
)

func (p statsPeriod) headers() []string {
	return []string{p.header, "Sessions", "Turns", "Input", "Cache Read", "Cache Write", "Output", "Cost"}
}

// statsProjectResult reports how one project database contributed.
type statsProjectResult struct {
	Path string `json:"path"`
	// Ledger is false when the database predates per-call tracking; all
	// of its spend is then approximate.
	Ledger bool `json:"ledger"`
	// Skipped is set when the database could not be read.
	Skipped bool   `json:"skipped"`
	Error   string `json:"error,omitempty"`
}

// periodOutput is the merged result across projects for one granularity.
// Each command wraps it in its own JSON type so the period list carries a
// self-describing key ("days", "months").
type periodOutput struct {
	Since    string
	Periods  []usage.Day
	Total    usage.Day
	Projects []statsProjectResult
}

type statsTarget struct {
	dbPath     string
	projectDir string
}

// runStatsPeriod is the body shared by the daily and monthly commands once
// the caller has resolved the reporting window. emptyMsg is shown when
// nothing was recorded; toJSON shapes the machine-readable output.
func runStatsPeriod(cmd *cobra.Command, period statsPeriod, since time.Time, emptyMsg string, toJSON func(periodOutput) any) error {
	ctx := cmd.Context()
	jsonOutput, _ := cmd.Flags().GetBool("json")
	currentOnly, _ := cmd.Flags().GetBool("current")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	targets, err := statsTargets(cmd, dataDir, currentOnly)
	if err != nil {
		return err
	}

	out, err := gatherPeriodUsage(ctx, period, targets, since, time.Local)
	if err != nil {
		return err
	}

	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetEscapeHTML(false)
		return enc.Encode(toJSON(out))
	}

	for _, p := range out.Projects {
		if p.Skipped {
			fmt.Fprintf(cmd.ErrOrStderr(), "warn: skipping %s: %s\n", p.Path, p.Error)
		}
	}

	if len(out.Periods) == 0 {
		cmd.Println(emptyMsg)
		return nil
	}

	if term.IsTerminal(os.Stdout.Fd()) {
		renderPeriodTable(os.Stdout, period, out)
		return nil
	}
	renderPeriodPlain(cmd.OutOrStdout(), period, out)
	return nil
}

// statsTargets resolves which project databases to read. By default every
// project in projects.json plus the current project's data directory are
// included; --current narrows it to the current project only.
func statsTargets(cmd *cobra.Command, dataDir string, currentOnly bool) ([]statsTarget, error) {
	var targets []statsTarget
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
		targets = append(targets, statsTarget{dbPath: dbPath, projectDir: projectDir})
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

// gatherPeriodUsage reads every target database in parallel and merges
// the per-period results. Unreadable databases are reported, not fatal.
func gatherPeriodUsage(ctx context.Context, period statsPeriod, targets []statsTarget, since time.Time, loc *time.Location) (periodOutput, error) {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		groups  [][]usage.Day
		results []statsProjectResult
	)
	sem := make(chan struct{}, 10)

	for _, target := range targets {
		wg.Add(1)
		go func(target statsTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result := statsProjectResult{Path: target.projectDir}
			periods, hasLedger, err := readPeriodUsage(ctx, period, target.dbPath, since, loc)
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
				groups = append(groups, periods)
			}
		}(target)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })

	merged := usage.MergeDays(groups...)
	out := periodOutput{
		Periods:  merged,
		Total:    usage.Total(merged),
		Projects: results,
	}
	if !since.IsZero() {
		out.Since = since.In(loc).Format(period.layout)
	}
	return out, nil
}

func readPeriodUsage(ctx context.Context, period statsPeriod, dbPath string, since time.Time, loc *time.Location) ([]usage.Day, bool, error) {
	conn, err := db.ConnectReadOnly(ctx, dbPath)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	return period.read(ctx, conn, since, loc)
}

func periodRow(label string, d usage.Day) []string {
	return []string{
		label,
		fmt.Sprintf("%d", d.Sessions),
		formatUsageTurns(d),
		formatUsageTokens(d.InputTokens, d),
		formatUsageTokens(d.CacheReadTokens, d),
		formatUsageTokens(d.CacheCreationTokens, d),
		formatUsageTokens(d.OutputTokens, d),
		formatUsageCost(d),
	}
}

func renderPeriodTable(w io.Writer, period statsPeriod, out periodOutput) {
	current := time.Now().In(time.Local).Format(period.layout)
	totalRow := len(out.Periods)

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(charmtone.Malibu).Padding(0, 1)
	cellStyle := lipgloss.NewStyle().Padding(0, 1)
	numberStyle := cellStyle.Align(lipgloss.Right)
	currentStyle := lipgloss.NewStyle().Bold(true).Foreground(charmtone.Guac)
	totalStyle := lipgloss.NewStyle().Bold(true)

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderRow(false).
		Headers(period.headers()...).
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
			case row >= 0 && row < len(out.Periods) && out.Periods[row].Date == current:
				return style.Inherit(currentStyle)
			}
			return style
		})

	for _, d := range out.Periods {
		t.Row(periodRow(d.Date, d)...)
	}
	t.Row(periodRow("Total", out.Total)...)

	lipgloss.Fprintln(w, t)
	if out.Total.Approximate {
		note := lipgloss.NewStyle().Foreground(charmtone.Squid).Render(fmt.Sprintf(
			"~ includes spend recorded before per-call tracking, attributed to each session's creation %s.", period.noun))
		lipgloss.Fprintln(w, note)
	}
}

func renderPeriodPlain(w io.Writer, period statsPeriod, out periodOutput) {
	fmt.Fprintln(w, strings.Join(period.headers(), "\t"))
	for _, d := range out.Periods {
		fmt.Fprintln(w, strings.Join(periodRow(d.Date, d), "\t"))
	}
	fmt.Fprintln(w, strings.Join(periodRow("Total", out.Total), "\t"))
}

func formatUsageCost(d usage.Day) string {
	cost := fmt.Sprintf("$%.2f", d.Cost)
	if d.Approximate {
		return "~" + cost
	}
	return cost
}

// formatUsageTurns prints the number of recorded calls. Periods whose
// spend is entirely approximate have no call records, so a dash is shown
// instead of a misleading zero.
func formatUsageTurns(d usage.Day) string {
	if d.Turns == 0 && d.Approximate {
		return "-"
	}
	return fmt.Sprintf("%d", d.Turns)
}

func formatUsageTokens(n int64, d usage.Day) string {
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
