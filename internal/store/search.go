package store

import (
	"database/sql"
	"regexp"
	"strings"
)

// Event search uses an FTS5 index with the trigram tokenizer, so any
// substring of 3+ characters matches (case-insensitively) without scanning
// every payload. Each event's document is its topic:code, description,
// source IP, payload and the names of the rules it ran; triggers keep it in
// step with the events and runs tables. Shorter terms, and databases where
// FTS5 isn't available, fall back to LIKE.
const ftsSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(body, tokenize='trigram');
CREATE TRIGGER IF NOT EXISTS events_fts_insert AFTER INSERT ON events BEGIN
	INSERT INTO events_fts(rowid, body) VALUES (new.id, new.topic || ':' || new.code || ' ' || new.description || ' ' || new.source_ip || ' ' || new.payload);
END;
CREATE TRIGGER IF NOT EXISTS events_fts_delete AFTER DELETE ON events BEGIN
	DELETE FROM events_fts WHERE rowid = old.id;
END;
CREATE TRIGGER IF NOT EXISTS events_fts_run AFTER INSERT ON runs BEGIN
	UPDATE events_fts SET body = body || ' ' || new.rule_name WHERE rowid = new.event_row;
END;
`

// setupFTS creates the index, filling it from existing events the first
// time. An error means search falls back to LIKE.
func setupFTS(db *sql.DB) error {
	if _, err := db.Exec(ftsSchema); err != nil {
		return err
	}
	var indexed, events int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM events_fts), (SELECT count(*) FROM events)`).Scan(&indexed, &events); err != nil {
		return err
	}
	if indexed == 0 && events > 0 {
		_, err := db.Exec(`INSERT INTO events_fts(rowid, body)
			SELECT e.id, e.topic || ':' || e.code || ' ' || e.description || ' ' || e.source_ip || ' ' || e.payload || ' ' ||
				COALESCE((SELECT group_concat(rule_name, ' ') FROM runs WHERE runs.event_row = e.id), '')
			FROM events e`)
		return err
	}
	return nil
}

// searchTerm is one piece of a search: free text, or key:value.
type searchTerm struct{ key, value string }

var termKey = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]{0,63}):`)

// parseSearch splits a query into terms. "quoted phrases" stay together,
// in free text and in values (site:"Head office"). A word whose colon is
// followed by "/" is free text, so URLs (http://…) aren't read as keys.
func parseSearch(s string) []searchTerm {
	var out []searchTerm
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		var t searchTerm
		if m := termKey.FindStringSubmatch(s); m != nil && len(s) > len(m[0]) && !strings.ContainsRune(" \t/", rune(s[len(m[0])])) {
			t.key = m[1]
			s = s[len(m[0]):]
		}
		if s[0] == '"' {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				t.value, s = s[1:], ""
			} else {
				t.value, s = s[1:end+1], s[end+2:]
			}
		} else {
			end := strings.IndexAny(s, " \t")
			if end < 0 {
				end = len(s)
			}
			t.value, s = s[:end], s[end:]
		}
		if t.value = strings.TrimSpace(t.value); t.value != "" {
			out = append(out, t)
		}
	}
	return out
}

// Search keys and the eventData field each one reads. Any other key is
// treated as an eventData field name (e.g. groupName:west).
var dataKeys = map[string]string{"device": "deviceName", "site": "siteName", "group": "groupName", "peripheral": "peripheralName"}

func likeArg(s string) string { return "%" + likeEscaper.Replace(s) + "%" }

// searchSQL turns terms into WHERE conditions (each starting with " AND").
func (s *Store) searchSQL(terms []searchTerm) (string, []any) {
	var (
		q    strings.Builder
		args []any
		fts  []string
	)
	for _, t := range terms {
		like := likeArg(t.value)
		switch strings.ToLower(t.key) {
		case "":
			if s.fts && len([]rune(t.value)) >= 3 {
				fts = append(fts, `"`+strings.ReplaceAll(t.value, `"`, `""`)+`"`)
				continue
			}
			q.WriteString(` AND (topic || ':' || code LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\' OR source_ip LIKE ? ESCAPE '\' OR payload LIKE ? ESCAPE '\'
				OR EXISTS (SELECT 1 FROM runs WHERE runs.event_row = events.id AND runs.rule_name LIKE ? ESCAPE '\'))`)
			args = append(args, like, like, like, like, like)
		case "topic", "code":
			q.WriteString(` AND ` + strings.ToLower(t.key) + ` LIKE ? ESCAPE '\'`)
			args = append(args, like)
		case "rule":
			q.WriteString(` AND EXISTS (SELECT 1 FROM runs WHERE runs.event_row = events.id AND runs.rule_name LIKE ? ESCAPE '\')`)
			args = append(args, like)
		case "status":
			q.WriteString(` AND EXISTS (SELECT 1 FROM runs WHERE runs.event_row = events.id AND runs.status = ?)`)
			args = append(args, strings.ToLower(t.value))
		case "ip":
			q.WriteString(` AND source_ip LIKE ? ESCAPE '\'`)
			args = append(args, like)
		default:
			field := dataKeys[strings.ToLower(t.key)]
			if field == "" {
				field = t.key
			}
			// "device:vmd" is also how events are shown (topic:code), so a
			// term like that matches either.
			q.WriteString(` AND (CAST(json_extract(payload, ?) AS TEXT) LIKE ? ESCAPE '\' OR topic || ':' || code LIKE ? ESCAPE '\')`)
			args = append(args, "$.eventData."+field, like, likeArg(t.key+":"+t.value))
		}
	}
	if len(fts) > 0 {
		q.WriteString(` AND events.id IN (SELECT rowid FROM events_fts WHERE events_fts MATCH ?)`)
		args = append(args, strings.Join(fts, " AND "))
	}
	return q.String(), args
}
