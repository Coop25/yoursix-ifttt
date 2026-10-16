package rules

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// DefaultTimezone is used when a schedule doesn't name one.
const DefaultTimezone = "America/Chicago"

// Schedule is a named set of time windows that rules can pick to limit
// when they run (by ScheduleID). Events outside every window are logged but
// ignored.
type Schedule struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Windows     []Window `json:"windows"`
	// Timezone is an IANA name like "America/Chicago".
	Timezone      string    `json:"timezone"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedByName string    `json:"updatedByName,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Window is some days of the week between two times of day. A window whose
// From is later than its To runs overnight (22:00–06:00) and belongs to the
// day it starts on: Friday 22:00–06:00 covers early Saturday.
type Window struct {
	// Days are "mon" … "sun"; empty means every day.
	Days []string `json:"days,omitempty"`
	// From and To are "HH:MM"; both empty means all day.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

var weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Validate normalises the schedule in place.
func (s *Schedule) Validate() error {
	s.Name = strings.TrimSpace(s.Name)
	s.Description = strings.TrimSpace(s.Description)
	if s.Name == "" {
		return errors.New("name is required")
	}
	s.Timezone = strings.TrimSpace(s.Timezone)
	if s.Timezone == "" {
		s.Timezone = DefaultTimezone
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("unknown time zone %q", s.Timezone)
	}
	if len(s.Windows) == 0 {
		return errors.New("add at least one time window")
	}
	if len(s.Windows) > 50 {
		return errors.New("at most 50 time windows")
	}
	for i := range s.Windows {
		if err := s.Windows[i].validate(); err != nil {
			return fmt.Errorf("window %d: %w", i+1, err)
		}
	}
	return nil
}

func (w *Window) validate() error {
	seen := map[string]bool{}
	days := []string{}
	for _, d := range w.Days {
		d = strings.ToLower(strings.TrimSpace(d))
		if len(d) > 3 {
			d = d[:3]
		}
		if !slices.Contains(weekdays, d) {
			return fmt.Errorf("unknown day %q (use mon … sun)", d)
		}
		if !seen[d] {
			seen[d] = true
			days = append(days, d)
		}
	}
	if len(days) == 7 {
		days = nil // every day
	}
	w.Days = days
	w.From, w.To = strings.TrimSpace(w.From), strings.TrimSpace(w.To)
	if (w.From == "") != (w.To == "") {
		return errors.New("set both a start and an end time, or neither")
	}
	for _, t := range []string{w.From, w.To} {
		if _, err := clock(t); t != "" && err != nil {
			return err
		}
	}
	if w.From != "" && w.From == w.To {
		return errors.New("start and end are the same time; clear both times for all day")
	}
	return nil
}

// clock parses "HH:MM" into minutes after midnight.
func clock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("time %q must be HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Active reports whether t falls inside any of the schedule's windows.
func (s Schedule) Active(t time.Time) bool {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil || s.Timezone == "" {
		loc, _ = time.LoadLocation(DefaultTimezone)
	}
	t = t.In(loc)
	for _, w := range s.Windows {
		if w.active(t) {
			return true
		}
	}
	return false
}

func (w Window) active(t time.Time) bool {
	day := t.Weekday()
	if w.From != "" && w.From != w.To {
		from, _ := clock(w.From)
		to, _ := clock(w.To)
		now := t.Hour()*60 + t.Minute()
		switch {
		case from < to:
			if now < from || now >= to {
				return false
			}
		case now >= from:
			// Overnight window, evening part: today's window.
		case now < to:
			// Overnight window, morning part: it started yesterday.
			day = (day + 6) % 7
		default:
			return false
		}
	}
	return len(w.Days) == 0 || slices.Contains(w.Days, weekdays[day])
}
