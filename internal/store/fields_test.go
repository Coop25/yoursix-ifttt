package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"yoursix-ifttt/internal/yoursix"
)

func insertRaw(t *testing.T, st *Store, topic, code string, data map[string]any) {
	t.Helper()
	ev := yoursix.Event{
		EventTopic: topic, EventCode: code, EventDescription: "d", EventData: data,
	}
	raw, _ := json.Marshal(ev)
	if _, _, err := st.InsertEvent(context.Background(), ev, raw, "1.2.3.4", false); err != nil {
		t.Fatal(err)
	}
}

func fieldByName(fs []FieldStat, name string) *FieldStat {
	for i := range fs {
		if fs[i].Name == name {
			return &fs[i]
		}
	}
	return nil
}

func TestFieldStatsNarrowsAndCounts(t *testing.T) {
	st := openTest(t, filepath.Join(t.TempDir(), "f.db"))
	insertRaw(t, st, "device", "vmd", map[string]any{"deviceName": "CAM-01"})
	insertRaw(t, st, "device", "vmd", map[string]any{"deviceName": "CAM-01"})
	insertRaw(t, st, "device", "vmd", map[string]any{"deviceName": "CAM-02"})
	insertRaw(t, st, "Device", "VMD", map[string]any{"deviceName": "CAM-01"})
	insertRaw(t, st, "peripheral", "input_activated", map[string]any{"peripheralName": "Door"})

	ctx := context.Background()
	want := []ValueCount{{"CAM-01", 3}, {"CAM-02", 1}}
	for _, codes := range [][]string{nil, {"device:vmd"}, {"vmd"}} {
		fs, err := st.FieldStats(ctx, []string{"device"}, codes, 500)
		if err != nil {
			t.Fatal(err)
		}
		f := fieldByName(fs, "deviceName")
		if f == nil || f.Seen != 4 || !reflect.DeepEqual(f.Values, want) {
			t.Fatalf("codes %v: %+v", codes, f)
		}
		if fieldByName(fs, "peripheralName") != nil {
			t.Fatalf("codes %v: peripheralName leaked", codes)
		}
	}
	fs, _ := st.FieldStats(ctx, nil, []string{"peripheral:vmd"}, 500)
	if len(fs) != 0 {
		t.Fatalf("mismatched qualified code matched: %+v", fs)
	}
}

func TestFieldStatsSkipsComplexAndLong(t *testing.T) {
	st := openTest(t, filepath.Join(t.TempDir(), "f.db"))
	insertRaw(t, st, "device", "vmd", map[string]any{"obj": map[string]any{"a": 1}, "long": strings.Repeat("x", 101)})
	fs, err := st.FieldStats(context.Background(), nil, nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"obj", "long"} {
		f := fieldByName(fs, name)
		if f == nil || f.Seen != 1 || len(f.Values) != 0 {
			t.Fatalf("%s: %+v", name, f)
		}
	}
}

func TestFieldStatsCapsValues(t *testing.T) {
	st := openTest(t, filepath.Join(t.TempDir(), "f.db"))
	for i := 0; i < 25; i++ {
		insertRaw(t, st, "device", "vmd", map[string]any{"n": i})
	}
	fs, err := st.FieldStats(context.Background(), nil, nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	if f := fieldByName(fs, "n"); f == nil || f.Seen != 25 || len(f.Values) != 20 {
		t.Fatalf("%+v", f)
	}
}
