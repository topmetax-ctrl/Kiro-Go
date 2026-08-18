package apikey

import (
	"fmt"
	"time"
)

func periodKey(policy string, t time.Time) string {
	t = t.UTC()
	switch policy {
	case ResetDaily:
		return t.Format("2006-01-02")
	case ResetWeekly:
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	case ResetMonthly:
		return t.Format("2006-01")
	default:
		return PeriodLifetime
	}
}

func periodBounds(policy string, t time.Time) (start time.Time, end *time.Time) {
	t = t.UTC()
	switch policy {
	case ResetDaily:
		s := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		e := s.AddDate(0, 0, 1)
		return s, &e
	case ResetWeekly:
		// ISO week starts Monday.
		wd := int(t.Weekday())
		if wd == 0 {
			wd = 7
		}
		s := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(wd - 1))
		e := s.AddDate(0, 0, 7)
		return s, &e
	case ResetMonthly:
		s := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		e := s.AddDate(0, 1, 0)
		return s, &e
	default:
		return t.UTC(), nil
	}
}

func usagePeriodFor(policy string, t time.Time) string {
	if policy == "" || policy == ResetLifetime {
		return PeriodLifetime
	}
	return periodKey(policy, t)
}
