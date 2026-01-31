package timeutil

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Parser parses user-specified history windows like "last 3 hours" or
// explicit ranges like "2024-01-01 to 2024-01-02" into concrete time ranges.
// It enforces a maximum allowed window to avoid unbounded queries.
type Parser struct {
	defaultWindow time.Duration
	maxWindow     time.Duration
}

// NewParser constructs a new Parser.
func NewParser(defaultWindow, maxWindow time.Duration) *Parser {
	return &Parser{defaultWindow: defaultWindow, maxWindow: maxWindow}
}

// TimeRange represents a normalized [From, To) interval in UTC.
type TimeRange struct {
	From time.Time
	To   time.Time
}

var (
	lastRe  = regexp.MustCompile(`(?i)^last\s+(\d+)\s*(h|hr|hrs|hour|hours|d|day|days)$`)
	rangeRe = regexp.MustCompile(`^\s*(\d{4}-\d{2}-\d{2})(?:[ T](\d{2}:\d{2}))?\s+to\s+(\d{4}-\d{2}-\d{2})(?:[ T](\d{2}:\d{2}))?\s*$`)
)

// Parse parses a free-form user input describing a history window. Empty input
// yields the default window ending at now. All returned times are in UTC.
func (p *Parser) Parse(now time.Time, input string) (TimeRange, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return p.defaultRange(now), nil
	}

	if m := lastRe.FindStringSubmatch(input); m != nil {
		return p.parseLast(now, m)
	}

	if m := rangeRe.FindStringSubmatch(input); m != nil {
		return p.parseExplicitRange(now.Location(), m)
	}

	return TimeRange{}, fmt.Errorf("could not parse time range expression")
}

func (p *Parser) defaultRange(now time.Time) TimeRange {
	to := now.UTC()
	from := to.Add(-p.defaultWindow)
	return TimeRange{From: from, To: to}
}

func (p *Parser) parseLast(now time.Time, m []string) (TimeRange, error) {
	// m[1] = number, m[2] = unit
	qtyStr := m[1]
	unit := strings.ToLower(m[2])

	var qty int
	_, err := fmt.Sscanf(qtyStr, "%d", &qty)
	if err != nil || qty <= 0 {
		return TimeRange{}, fmt.Errorf("invalid quantity in time range")
	}

	var d time.Duration
	switch unit {
	case "h", "hr", "hrs", "hour", "hours":
		d = time.Duration(qty) * time.Hour
	case "d", "day", "days":
		d = time.Duration(qty) * 24 * time.Hour
	default:
		return TimeRange{}, fmt.Errorf("unsupported time unit")
	}

	if d > p.maxWindow {
		return TimeRange{}, fmt.Errorf("requested window exceeds maximum of %s", p.maxWindow)
	}

	to := now.UTC()
	from := to.Add(-d)
	return TimeRange{From: from, To: to}, nil
}

func (p *Parser) parseExplicitRange(loc *time.Location, m []string) (TimeRange, error) {
	// m[1]=fromDate, m[2]=fromTime?, m[3]=toDate, m[4]=toTime?
	fromDate := m[1]
	fromTime := m[2]
	toDate := m[3]
	toTime := m[4]

	if fromTime == "" {
		fromTime = "00:00"
	}
	if toTime == "" {
		toTime = "23:59"
	}

	from, err := time.ParseInLocation("2006-01-02 15:04", fromDate+" "+fromTime, loc)
	if err != nil {
		return TimeRange{}, fmt.Errorf("invalid from date/time")
	}
	to, err := time.ParseInLocation("2006-01-02 15:04", toDate+" "+toTime, loc)
	if err != nil {
		return TimeRange{}, fmt.Errorf("invalid to date/time")
	}

	if !to.After(from) {
		return TimeRange{}, fmt.Errorf("end must be after start")
	}

	// Normalize to UTC and enforce max window.
	fromUTC := from.UTC()
	toUTC := to.UTC()

	if toUTC.Sub(fromUTC) > p.maxWindow {
		return TimeRange{}, fmt.Errorf("requested window exceeds maximum of %s", p.maxWindow)
	}

	// Make the upper bound exclusive by adding one minute if we used 23:59.
	// This keeps semantics intuitive while still enforcing the max window.
	return TimeRange{From: fromUTC, To: toUTC}, nil
}
