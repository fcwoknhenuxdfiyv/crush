// Package usage persists a per-API-call ledger of token usage and cost and
// aggregates it into daily totals. Sessions only carry a running cost
// total, which cannot say *when* money was spent; the ledger records each
// step as it happens so spend can be reported per day.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// Source identifies which agent code path produced a usage event.
type Source string

const (
	// SourceTurn is a regular step of a conversation turn.
	SourceTurn Source = "turn"
	// SourceSummarize is a context compaction call.
	SourceSummarize Source = "summarize"
	// SourceTitle is a session title generation call.
	SourceTitle Source = "title"
)

// Event is a single provider call worth of usage.
type Event struct {
	SessionID           string
	Provider            string
	Model               string
	Source              Source
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// Cost is the amount actually charged to the session for this call,
	// after any provider override or flat-rate zeroing.
	Cost float64
	// Estimated marks usage that was inferred because the provider did not
	// report it. Estimated events carry no cost.
	Estimated bool
}

// Recorder persists usage events. A nil Recorder is a valid no-op.
type Recorder interface {
	Record(ctx context.Context, ev Event) error
}

// Service is the production Recorder backed by the session database.
type Service interface {
	Recorder
}

type service struct {
	q   db.Querier
	now func() time.Time
}

// NewService returns a Recorder that writes to the given queries.
func NewService(q db.Querier) Service {
	return &service{q: q, now: time.Now}
}

func (s *service) Record(ctx context.Context, ev Event) error {
	if ev.SessionID == "" {
		return errors.New("usage event requires a session id")
	}
	if ev.Source == "" {
		ev.Source = SourceTurn
	}
	var estimated int64
	if ev.Estimated {
		estimated = 1
	}
	return s.q.CreateUsageEvent(ctx, db.CreateUsageEventParams{
		SessionID:           ev.SessionID,
		Provider:            ev.Provider,
		Model:               ev.Model,
		Source:              string(ev.Source),
		InputTokens:         ev.InputTokens,
		OutputTokens:        ev.OutputTokens,
		CacheReadTokens:     ev.CacheReadTokens,
		CacheCreationTokens: ev.CacheCreationTokens,
		Cost:                ev.Cost,
		Estimated:           estimated,
		CreatedAt:           s.now().Unix(),
	})
}

// Day is the usage aggregated over one calendar day.
type Day struct {
	// Date is the calendar day in YYYY-MM-DD form, in the location the
	// aggregation was computed for.
	Date                string `json:"date"`
	Sessions            int64  `json:"sessions"`
	Turns               int64  `json:"turns"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	// Cost is the total spend for the day, including ApproximateCost.
	Cost float64 `json:"cost"`
	// ApproximateCost is the part of Cost that could not be attributed to
	// a specific call because it predates the ledger. It is attributed to
	// the day the session was created.
	ApproximateCost float64 `json:"approximate_cost"`
	// Approximate reports whether any part of Cost is approximate.
	Approximate bool `json:"approximate"`

	sessions map[string]struct{}
}

// DateLayout is the format used for Day.Date.
const DateLayout = "2006-01-02"

// costEpsilon absorbs floating point drift between a session's running
// total and the sum of its ledger rows.
const costEpsilon = 1e-6

// DailyFromDB aggregates usage from one project database into days, in
// the given location. since limits the range (zero means all time).
//
// Ledger events are bucketed by the time they happened. Spend recorded on
// sessions before the ledger existed is attributed to the session's
// creation day and flagged approximate. Databases that predate the ledger
// table entirely are handled the same way; hasLedger reports whether the
// table was present.
func DailyFromDB(ctx context.Context, conn *sql.DB, since time.Time, loc *time.Location) (days []Day, hasLedger bool, err error) {
	if loc == nil {
		loc = time.Local
	}
	var sinceUnix int64
	if !since.IsZero() {
		sinceUnix = since.Unix()
	}

	q := db.New(conn)
	byDate := map[string]*Day{}
	bucket := func(createdAt int64) *Day {
		date := time.Unix(createdAt, 0).In(loc).Format(DateLayout)
		d, ok := byDate[date]
		if !ok {
			d = &Day{Date: date, sessions: map[string]struct{}{}}
			byDate[date] = d
		}
		return d
	}

	events, err := q.ListUsageEventsSince(ctx, sinceUnix)
	switch {
	case err == nil:
		hasLedger = true
		for _, ev := range events {
			d := bucket(ev.CreatedAt)
			d.Turns++
			d.InputTokens += ev.InputTokens
			d.OutputTokens += ev.OutputTokens
			d.CacheReadTokens += ev.CacheReadTokens
			d.CacheCreationTokens += ev.CacheCreationTokens
			d.Cost += ev.Cost
			d.sessions[ev.SessionID] = struct{}{}
		}
	case isMissingTable(err):
		hasLedger = false
	default:
		return nil, false, fmt.Errorf("listing usage events: %w", err)
	}

	if hasLedger {
		rows, err := q.ListRootSessionLedgerCost(ctx, sinceUnix)
		if err != nil {
			return nil, true, fmt.Errorf("listing session costs: %w", err)
		}
		for _, row := range rows {
			remainder := row.Cost - row.LedgerCost
			if remainder <= costEpsilon {
				continue
			}
			d := bucket(row.CreatedAt)
			d.Cost += remainder
			d.ApproximateCost += remainder
			d.Approximate = true
			d.sessions[row.ID] = struct{}{}
		}
	} else {
		rows, err := q.ListRootSessionCosts(ctx, sinceUnix)
		if err != nil {
			return nil, false, fmt.Errorf("listing sessions: %w", err)
		}
		for _, row := range rows {
			if row.Cost <= 0 {
				continue
			}
			d := bucket(row.CreatedAt)
			d.Cost += row.Cost
			d.ApproximateCost += row.Cost
			d.Approximate = true
			d.sessions[row.ID] = struct{}{}
		}
	}

	days = make([]Day, 0, len(byDate))
	for _, d := range byDate {
		d.Sessions = int64(len(d.sessions))
		days = append(days, *d)
	}
	sortDays(days)
	return days, hasLedger, nil
}

// MergeDays sums per-day usage from several databases. Session counts are
// summed, which is exact because session IDs never repeat across
// databases.
func MergeDays(groups ...[]Day) []Day {
	byDate := map[string]*Day{}
	for _, group := range groups {
		for _, in := range group {
			d, ok := byDate[in.Date]
			if !ok {
				d = &Day{Date: in.Date}
				byDate[in.Date] = d
			}
			d.add(in)
		}
	}
	days := make([]Day, 0, len(byDate))
	for _, d := range byDate {
		days = append(days, *d)
	}
	sortDays(days)
	return days
}

// Total folds days into a single Day with an empty Date.
func Total(days []Day) Day {
	var total Day
	for _, d := range days {
		total.add(d)
	}
	return total
}

func (d *Day) add(in Day) {
	d.Sessions += in.Sessions
	d.Turns += in.Turns
	d.InputTokens += in.InputTokens
	d.OutputTokens += in.OutputTokens
	d.CacheReadTokens += in.CacheReadTokens
	d.CacheCreationTokens += in.CacheCreationTokens
	d.Cost += in.Cost
	d.ApproximateCost += in.ApproximateCost
	d.Approximate = d.Approximate || in.Approximate
}

func sortDays(days []Day) {
	sort.Slice(days, func(i, j int) bool {
		return days[i].Date > days[j].Date
	})
}

func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such table") && strings.Contains(msg, "usage_events")
}
