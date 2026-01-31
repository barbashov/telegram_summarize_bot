package timeutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParser_DefaultRange(t *testing.T) {
	p := NewParser(24*time.Hour, 7*24*time.Hour)
	now := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)

	tr, err := p.Parse(now, "")
	require.NoError(t, err)
	require.Equal(t, now.Add(-24*time.Hour), tr.From)
	require.Equal(t, now, tr.To)
}

func TestParser_LastHours(t *testing.T) {
	p := NewParser(24*time.Hour, 7*24*time.Hour)
	now := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)

	tr, err := p.Parse(now, "last 6 hours")
	require.NoError(t, err)
	require.Equal(t, now.Add(-6*time.Hour), tr.From)
	require.Equal(t, now, tr.To)
}

func TestParser_ExplicitRange(t *testing.T) {
	p := NewParser(24*time.Hour, 7*24*time.Hour)
	now := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)

	tr, err := p.Parse(now, "2024-01-01 to 2024-01-02")
	require.NoError(t, err)
	require.True(t, tr.To.After(tr.From))
}

func TestParser_ExceedsMaxWindow(t *testing.T) {
	p := NewParser(24*time.Hour, 24*time.Hour)
	now := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)

	_, err := p.Parse(now, "last 3 days")
	require.Error(t, err)
}
