package rules

import (
	"testing"
	"time"
)

func TestScheduleActive(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04 MST", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	utc := func(w ...Window) Schedule { return Schedule{Timezone: "UTC", Windows: w} }
	// 2026-10-16 is a Friday.
	cases := []struct {
		name string
		s    Schedule
		t    string
		want bool
	}{
		{"all day, any day", utc(Window{}), "2026-10-16 03:00 UTC", true},
		{"no windows is never", utc(), "2026-10-16 03:00 UTC", false},
		{"weekday window inside", utc(Window{Days: []string{"fri"}, From: "09:00", To: "17:00"}), "2026-10-16 09:00 UTC", true},
		{"end is exclusive", utc(Window{From: "09:00", To: "17:00"}), "2026-10-16 17:00 UTC", false},
		{"wrong day", utc(Window{Days: []string{"mon"}, From: "09:00", To: "17:00"}), "2026-10-16 10:00 UTC", false},
		{"overnight evening", utc(Window{Days: []string{"fri"}, From: "22:00", To: "06:00"}), "2026-10-16 23:30 UTC", true},
		{"overnight morning belongs to previous day", utc(Window{Days: []string{"fri"}, From: "22:00", To: "06:00"}), "2026-10-17 05:59 UTC", true},
		{"overnight morning, previous day not listed", utc(Window{Days: []string{"sat"}, From: "22:00", To: "06:00"}), "2026-10-17 05:00 UTC", false},
		{"overnight gap", utc(Window{From: "22:00", To: "06:00"}), "2026-10-16 12:00 UTC", false},
		{"any window", utc(Window{Days: []string{"mon"}}, Window{Days: []string{"sat", "sun"}}), "2026-10-18 12:00 UTC", true},
		{"time zone", Schedule{Timezone: "America/New_York", Windows: []Window{{From: "09:00", To: "10:00"}}}, "2026-10-16 13:30 UTC", true},
		// No zone means America/Chicago (CDT, UTC-5, in October).
		{"default zone", Schedule{Windows: []Window{{From: "09:00", To: "10:00"}}}, "2026-10-16 14:30 UTC", true},
	}
	for _, c := range cases {
		if got := c.s.Active(at(c.t)); got != c.want {
			t.Errorf("%s: Active(%s) = %v", c.name, c.t, got)
		}
	}
}

func TestScheduleValidate(t *testing.T) {
	good := Schedule{Name: " Nights ", Windows: []Window{
		{Days: []string{"Monday", "tue", "mon"}, From: "22:00", To: "06:00"},
		{Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}},
	}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if good.Name != "Nights" || good.Timezone != DefaultTimezone {
		t.Errorf("not normalised: %+v", good)
	}
	if d := good.Windows[0].Days; len(d) != 2 || d[0] != "mon" {
		t.Errorf("days not normalised: %v", d)
	}
	if good.Windows[1].Days != nil {
		t.Errorf("all seven days should mean every day: %v", good.Windows[1].Days)
	}
	for _, bad := range []Schedule{
		{Windows: []Window{{}}},
		{Name: "x"},
		{Name: "x", Windows: []Window{{Days: []string{"funday"}}}},
		{Name: "x", Windows: []Window{{From: "09:00"}}},
		{Name: "x", Windows: []Window{{From: "9am", To: "10:00"}}},
		{Name: "x", Windows: []Window{{From: "09:00", To: "09:00"}}},
		{Name: "x", Timezone: "Mars/Olympus", Windows: []Window{{}}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v: want error", bad)
		}
	}
}
