package cmd

import (
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/usage"
	"github.com/spf13/cobra"
)

var statsMonthlyCmd = &cobra.Command{
	Use:   "monthly",
	Short: "Show spend per month across all projects",
	Long: `Show token usage and cost per calendar month, summed across every
project Crush knows about.

Each provider call is recorded as it happens, so months are exact from
the version that introduced the ledger onward. Spend recorded before that
is attributed to the month its session was created and marked with "~".`,
	Example: `
# Last 12 months across all known projects
crush stats monthly

# This quarter, machine readable
crush stats monthly --months 3 --json

# Only the project in the current directory
crush stats monthly --current
  `,
	RunE: runStatsMonthly,
}

func init() {
	statsMonthlyCmd.Flags().Int("months", 12, "Number of months to include (0 for all time)")
	statsMonthlyCmd.Flags().Bool("json", false, "Output as JSON")
	statsMonthlyCmd.Flags().Bool("current", false, "Only include the project in the current directory")
	statsCmd.AddCommand(statsMonthlyCmd)
}

type monthlyOutput struct {
	Since    string               `json:"since,omitempty"`
	Months   []usage.Day          `json:"months"`
	Total    usage.Day            `json:"total"`
	Projects []statsProjectResult `json:"projects"`
}

func runStatsMonthly(cmd *cobra.Command, _ []string) error {
	months, _ := cmd.Flags().GetInt("months")
	if months < 0 {
		return fmt.Errorf("--months must be zero or positive")
	}

	var since time.Time
	emptyMsg := "No usage recorded."
	if months > 0 {
		since = monthlySince(time.Now(), months)
		emptyMsg = fmt.Sprintf("No usage recorded in the last %d months.", months)
	}

	return runStatsPeriod(cmd, monthlyPeriod, since, emptyMsg, func(out periodOutput) any {
		return monthlyOutput{
			Since:    out.Since,
			Months:   out.Periods,
			Total:    out.Total,
			Projects: out.Projects,
		}
	})
}

// monthlySince returns the first instant of the month that starts a
// window of n calendar months ending with the month containing now, in
// now's location. The current month always counts as one of the n.
func monthlySince(now time.Time, n int) time.Time {
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return startOfMonth.AddDate(0, -(n - 1), 0)
}
