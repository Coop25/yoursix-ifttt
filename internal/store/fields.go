package store

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// maxFieldValueLen is the longest string offered as a suggested value.
const maxFieldValueLen = 100

// maxFieldValues is how many values are kept per field.
const maxFieldValues = 20

type ValueCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type FieldStat struct {
	Name   string       `json:"name"`
	Seen   int          `json:"seen"`
	Values []ValueCount `json:"values"`
}

// FieldStats summarises the property names, and the most common values, found
// in the latest limit stored events. topics and codes narrow the events the
// way a rule trigger does: a bare code matches any topic, "topic:code" only
// that topic. Empty lists match everything.
func (s *Store) FieldStats(ctx context.Context, topics, codes []string, limit int) ([]FieldStat, error) {
	var (
		where []string
		args  []any
	)
	if len(topics) > 0 {
		where = append(where, "lower(topic) IN (?"+strings.Repeat(",?", len(topics)-1)+")")
		for _, t := range topics {
			args = append(args, strings.ToLower(t))
		}
	}
	if len(codes) > 0 {
		var ors []string
		for _, c := range codes {
			c = strings.ToLower(c)
			if topic, code, ok := strings.Cut(c, ":"); ok {
				ors = append(ors, "(lower(topic) = ? AND lower(code) = ?)")
				args = append(args, topic, code)
			} else {
				ors = append(ors, "lower(code) = ?")
				args = append(args, c)
			}
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	q := `SELECT payload FROM events`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type acc struct {
		seen   int
		values map[string]int
	}
	fields := map[string]*acc{}
	add := func(name string, v any) {
		a := fields[name]
		if a == nil {
			a = &acc{values: map[string]int{}}
			fields[name] = a
		}
		a.seen++
		switch x := v.(type) {
		case string:
			if len(x) <= maxFieldValueLen {
				a.values[x]++
			}
		case float64:
			a.values[strconv.FormatFloat(x, 'f', -1, 64)]++
		case bool:
			a.values[strconv.FormatBool(x)]++
		}
	}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		for _, k := range []string{"eventCode", "eventTopic", "eventDescription"} {
			if v, ok := ev[k]; ok {
				add(k, v)
			}
		}
		if data, ok := ev["eventData"].(map[string]any); ok {
			for k, v := range data {
				add(k, v)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]FieldStat, 0, len(fields))
	for name, a := range fields {
		fs := FieldStat{Name: name, Seen: a.seen, Values: []ValueCount{}}
		for v, c := range a.values {
			fs.Values = append(fs.Values, ValueCount{v, c})
		}
		sort.Slice(fs.Values, func(i, j int) bool {
			if fs.Values[i].Count != fs.Values[j].Count {
				return fs.Values[i].Count > fs.Values[j].Count
			}
			return fs.Values[i].Value < fs.Values[j].Value
		})
		if len(fs.Values) > maxFieldValues {
			fs.Values = fs.Values[:maxFieldValues]
		}
		out = append(out, fs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seen != out[j].Seen {
			return out[i].Seen > out[j].Seen
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
