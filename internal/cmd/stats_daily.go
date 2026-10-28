package cmd

import (
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/usage"
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

type dailyOutput struct {
	Since    string               `json:"since,omitempty"`
	Days     []usage.Day          `json:"days"`
	Total    usage.Day            `json:"total"`
	Projects []statsProjectResult `json:"projects"`
}

func runStatsDaily(cmd *cobra.Command, _ []string) error {
	days, _ := cmd.Flags().GetInt("days")
	if days < 0 {
		return fmt.Errorf("--days must be zero or positive")
	}

	var since time.Time
	emptyMsg := "No usage recorded."
	if days > 0 {
		now := time.Now()
		startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		since = startOfToday.AddDate(0, 0, -(days - 1))
		emptyMsg = fmt.Sprintf("No usage recorded in the last %d days.", days)
	}

	return runStatsPeriod(cmd, dailyPeriod, since, emptyMsg, func(out periodOutput) any {
		return dailyOutput{
			Since:    out.Since,
			Days:     out.Periods,
			Total:    out.Total,
			Projects: out.Projects,
		}
	})
}
