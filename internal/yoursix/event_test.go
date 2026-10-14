package yoursix

import "testing"

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
