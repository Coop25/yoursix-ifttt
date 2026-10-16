package actions

import (
	"context"
	"fmt"
	"sort"

	"yoursix-ifttt/internal/rules"
)

// PreviewField is one templated value of an action, rendered against an
// event. Error is set instead of Value when the template fails.
type PreviewField struct {
	Field string `json:"field"`
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

// maskSecrets stands in for {{ secret }} in previews, so rendering never
// reveals (or needs) a secret's value.
func maskSecrets(name string) (string, error) { return "‹secret " + name + "›", nil }

// Preview renders an action's templated values with data, without running
// it. A saved action ("request") is resolved, its parameters rendered, and
// its own values rendered with them.
func (r *Runner) Preview(ctx context.Context, a rules.Action, data map[string]any) []PreviewField {
	out := []PreviewField{}
	add := func(field, tmpl string, lk SecretLookup) {
		if tmpl == "" {
			return
		}
		v, err := Render(tmpl, data, lk)
		if err != nil {
			out = append(out, PreviewField{Field: field, Error: err.Error()})
			return
		}
		out = append(out, PreviewField{Field: field, Value: v})
	}
	switch a.Type {
	case rules.ActionHTTP, "":
		add("URL", a.URL, maskSecrets)
		for _, k := range sortedKeys(a.Headers) {
			add("Header "+k, a.Headers[k], maskSecrets)
		}
		add("Body", a.Body, maskSecrets)
		add("Password", a.Password, maskSecrets)
	case rules.ActionDiscord:
		add("Webhook URL", a.WebhookURL, maskSecrets)
		add("Message", a.Message, maskSecrets)
		if d := a.Discord; d != nil {
			add("Username", d.Username, maskSecrets)
			add("Avatar URL", d.AvatarURL, maskSecrets)
			add("Thread name", d.ThreadName, maskSecrets)
			for i, e := range d.Embeds {
				p := fmt.Sprintf("Embed %d ", i+1)
				add(p+"title", e.Title, maskSecrets)
				add(p+"link", e.URL, maskSecrets)
				add(p+"description", e.Description, maskSecrets)
				add(p+"timestamp", e.Timestamp, maskSecrets)
				add(p+"author", e.AuthorName, maskSecrets)
				add(p+"footer", e.FooterText, maskSecrets)
				add(p+"image", e.ImageURL, maskSecrets)
				add(p+"thumbnail", e.ThumbnailURL, maskSecrets)
				for _, f := range e.Fields {
					add(p+"field "+f.Name, f.Value, maskSecrets)
				}
			}
		}
	case rules.ActionRequest:
		if r.Requests == nil {
			return []PreviewField{{Field: "Saved action", Error: "saved actions are not available"}}
		}
		sr, err := r.Requests.SavedRequest(ctx, a.RequestID)
		if err != nil {
			return []PreviewField{{Field: "Saved action", Error: fmt.Sprintf("saved action #%d: %v", a.RequestID, err)}}
		}
		given := map[string]string{}
		for _, k := range sortedKeys(a.Params) {
			v, err := Render(a.Params[k], data, noSecrets)
			if err != nil {
				out = append(out, PreviewField{Field: "Parameter " + k, Error: err.Error()})
				continue
			}
			given[k] = v
		}
		params, err := sr.ResolveParams(given)
		if err != nil {
			return append(out, PreviewField{Field: "Parameters", Error: err.Error()})
		}
		for _, p := range sr.Params {
			out = append(out, PreviewField{Field: "Parameter " + p.Name, Value: params[p.Name]})
		}
		inner := make(map[string]any, len(data)+1)
		for k, v := range data {
			inner[k] = v
		}
		inner["params"] = params
		return append(out, r.Preview(ctx, sr.Request, inner)...)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
