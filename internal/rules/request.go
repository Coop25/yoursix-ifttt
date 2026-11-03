package rules

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var validParam = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// SavedRequest is a reusable "saved action" (an HTTP request, delay or
// Discord message) that rules run through an ActionRequest. Its templated
// strings see the event (.data, .event) plus .params, the values a rule
// supplies.
type SavedRequest struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Params      []Param `json:"params"`
	// Request is the action it runs: an ActionHTTP, ActionDelay or
	// ActionDiscord, never another ActionRequest.
	Request Action `json:"request"`
	// UpdatedBy is the admin who last saved it. A request that uses secrets
	// runs with that admin's secrets, so only that admin's rules may use it.
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedByName string    `json:"updatedByName,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Param declares a value rules can fill in. Without a Default, rules must
// provide it.
type Param struct {
	Name        string `json:"name"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

func (p Param) Required() bool { return p.Default == "" }

// Validate normalises the saved request in place.
func (s *SavedRequest) Validate() error {
	s.Name = strings.TrimSpace(s.Name)
	s.Description = strings.TrimSpace(s.Description)
	if s.Name == "" {
		return errors.New("name is required")
	}
	if s.Request.Type == "" {
		s.Request.Type = ActionHTTP
	}
	switch s.Request.Type {
	case ActionHTTP, ActionDelay, ActionDiscord:
	default:
		return fmt.Errorf("a saved action can't be of type %q", s.Request.Type)
	}
	// The rule using it supplies the label and error handling.
	s.Request.Name, s.Request.ContinueOnError = "", false
	if err := s.Request.validate(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range s.Params {
		p := &s.Params[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Description = strings.TrimSpace(p.Description)
		if !validParam.MatchString(p.Name) {
			return fmt.Errorf("parameter %d: names use letters, digits and _ and can't start with a digit", i+1)
		}
		if seen[p.Name] {
			return fmt.Errorf("parameter %q is declared twice", p.Name)
		}
		seen[p.Name] = true
	}
	if s.Params == nil {
		s.Params = []Param{}
	}
	return nil
}

// ResolveParams merges declared defaults with the values a rule supplied
// (already rendered), rejecting unknown names and missing required values.
func (s SavedRequest) ResolveParams(given map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(s.Params))
	declared := map[string]bool{}
	for _, p := range s.Params {
		declared[p.Name] = true
		out[p.Name] = p.Default
	}
	for k, v := range given {
		if !declared[k] {
			return nil, fmt.Errorf("saved action %q has no parameter %q", s.Name, k)
		}
		if v != "" {
			out[k] = v
		}
	}
	for _, p := range s.Params {
		if out[p.Name] == "" && p.Required() {
			return nil, fmt.Errorf("saved action %q needs a value for %q", s.Name, p.Name)
		}
	}
	return out, nil
}
