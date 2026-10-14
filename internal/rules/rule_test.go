package rules

import (
	"testing"

	"yoursix-ifttt/internal/yoursix"
)

func TestTriggerMatches(t *testing.T) {
	ev := yoursix.Sample(yoursix.TopicDevice, "vmd")

	cases := []struct {
		name string
		t    Trigger
		want bool
	}{
		{"empty matches all", Trigger{}, true},
		{"topic", Trigger{Topics: []string{"device"}}, true},
		{"wrong topic", Trigger{Topics: []string{"sensor"}}, false},
		{"bare code", Trigger{Codes: []string{"aoa", "vmd"}}, true},
		{"qualified code", Trigger{Codes: []string{"device:vmd"}}, true},
		{"wrong qualified code", Trigger{Codes: []string{"barrier:vmd"}}, false},
		{"equals ci", Trigger{Filters: []Filter{{Field: "deviceName", Op: OpEquals, Value: "cam-01"}}}, true},
		{"not equals", Trigger{Filters: []Filter{{Field: "deviceName", Op: OpNotEquals, Value: "CAM-01"}}}, false},
		{"contains", Trigger{Filters: []Filter{{Field: "siteName", Op: OpContains, Value: "h"}}}, true},
		{"regex", Trigger{Filters: []Filter{{Field: "deviceName", Op: OpRegex, Value: `^CAM-\d+$`}}}, true},
		{"bool field", Trigger{Filters: []Filter{{Field: "groupActive", Op: OpEquals, Value: "true"}}}, true},
		{"exists", Trigger{Filters: []Filter{{Field: "videoSourceId", Op: OpExists}}}, true},
		{"not exists", Trigger{Filters: []Filter{{Field: "barrierId", Op: OpNotExists}}}, true},
		{"top-level field", Trigger{Filters: []Filter{{Field: "eventCode", Op: OpEquals, Value: "vmd"}}}, true},
		{"data prefix", Trigger{Filters: []Filter{{Field: "data.deviceName", Op: OpStartsWith, Value: "cam"}}}, true},
		{"all filters must pass", Trigger{Filters: []Filter{
			{Field: "deviceName", Op: OpEquals, Value: "CAM-01"},
			{Field: "siteName", Op: OpEquals, Value: "Elsewhere"},
		}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Rule{Name: "x", Trigger: c.t, Actions: []Action{{Type: ActionDelay, DelayMillis: 1}}}
			if err := r.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if got := r.Trigger.Matches(ev); got != c.want {
				t.Fatalf("Matches = %v, want %v", got, c.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	bad := []Rule{
		{Name: "", Actions: []Action{{Type: ActionDelay, DelayMillis: 1}}},
		{Name: "no actions"},
		{Name: "bad type", Actions: []Action{{Type: "shell"}}},
		{Name: "http no url", Actions: []Action{{Type: ActionHTTP}}},
		{Name: "bad auth", Actions: []Action{{Type: ActionHTTP, URL: "http://x", Auth: "ntlm"}}},
		{Name: "bad op", Trigger: Trigger{Filters: []Filter{{Field: "a", Op: "gt"}}}, Actions: []Action{{Type: ActionDelay, DelayMillis: 1}}},
		{Name: "bad regex", Trigger: Trigger{Filters: []Filter{{Field: "a", Op: OpRegex, Value: "("}}}, Actions: []Action{{Type: ActionDelay, DelayMillis: 1}}},
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("rule %q: expected validation error", r.Name)
		}
	}

	ok := Rule{Name: " ok ", Trigger: Trigger{Topics: []string{" Device "}}, Actions: []Action{{Type: ActionHTTP, URL: "http://x"}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Name != "ok" || ok.Trigger.Topics[0] != "device" || ok.Actions[0].Method != "GET" {
		t.Fatalf("not normalised: %+v", ok)
	}
}
