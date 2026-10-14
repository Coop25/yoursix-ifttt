package yoursix

import (
	"testing"
	"time"
)

func TestIsPlatformUserAgent(t *testing.T) {
	cases := map[string]bool{
		"postoffice/v2.21.0 y6-platform": true, // what YourSix actually sends
		"y6-platform":                    true, // what the docs describe
		"y6-platform/1.2.3":              true,
		"Y6-Platform":                    true,
		"foo y6-platform bar":            true,
		"":                               false,
		"curl/8.5.0":                     false,
		"not-y6-platform":                false,
		"y6-platformx":                   false,
		"postoffice/v2.21.0":             false,
	}
	for ua, want := range cases {
		if got := IsPlatformUserAgent(ua); got != want {
			t.Errorf("IsPlatformUserAgent(%q) = %v, want %v", ua, got, want)
		}
	}
}

func TestMatrixURL(t *testing.T) {
	at := time.UnixMilli(1791996197084)
	cases := []struct {
		name, device, source, group string
		at                          time.Time
		want                        string
	}{
		{
			// The link from a real event (videoSourceId "0"), as YourSix builds it.
			"camera event", "1f1c6657-3ccc-6874-b3ea-e3755baaf62d", "0", "cf84e116", at,
			"https://platform.yoursix.com/matrix?deviceIds=1f1c6657-3ccc-6874-b3ea-e3755baaf62d_1&timestamp=1791996197.084",
		},
		{"second source", "dev", "1", "", at, "https://platform.yoursix.com/matrix?deviceIds=dev_2&timestamp=1791996197.084"},
		{"no source: whole device", "dev", "", "grp", at, "https://platform.yoursix.com/matrix?deviceIds=dev&timestamp=1791996197.084"},
		{"non-numeric source ignored", "dev", "abc", "", at, "https://platform.yoursix.com/matrix?deviceIds=dev&timestamp=1791996197.084"},
		{"alarm group event", "", "", "grp-1", at, "https://platform.yoursix.com/matrix?alarmGroupId=grp-1&timestamp=1791996197.084"},
		{"no time: live", "dev", "0", "", time.Time{}, "https://platform.yoursix.com/matrix?deviceIds=dev_1"},
		{"nothing to open", "", "", "", at, ""},
	}
	for _, c := range cases {
		if got := MatrixURL("", c.device, c.source, c.group, c.at); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	if got := MatrixURL("https://eu.example.com/", "dev", "", "", time.Time{}); got != "https://eu.example.com/matrix?deviceIds=dev" {
		t.Errorf("custom base: %s", got)
	}
	// eventOccurred is RFC3339, possibly with fractional seconds.
	ev := Event{EventOccurred: "2026-10-14T16:43:17.084Z"}
	if got := MatrixURL("", "dev", "0", "", ev.OccurredAt()); got != "https://platform.yoursix.com/matrix?deviceIds=dev_1&timestamp=1791996197.084" {
		t.Errorf("from eventOccurred: %s", got)
	}
}
