package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"yoursix-ifttt/internal/actions"
	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/store"
)

const maxValueBytes = 8 << 10

var (
	// ErrInvalid wraps problems with the name or value the user supplied.
	ErrInvalid  = errors.New("invalid secret")
	ErrDisabled = errors.New("secrets manager is not configured: set Y6IFTTT_SECRETS_KEY (openssl rand -hex 32)")
	validName   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// Manager stores per-admin secrets encrypted and resolves them for runs.
//
// Access model: an admin can list (names only), set, replace and delete
// their own secrets. Nobody can read a value back through the API. A rule
// can only use secrets owned by the admin who last saved it, so another
// admin can't exfiltrate a secret by pointing someone else's rule at their
// own server: saving the rule makes it theirs.
type Manager struct {
	box   *Box // nil when no key is configured
	store *store.Store
}

func NewManager(box *Box, st *store.Store) *Manager { return &Manager{box: box, store: st} }

func (m *Manager) Enabled() bool { return m.box != nil }

// NormalizeName upper-cases and validates a secret name.
func NormalizeName(name string) (string, error) {
	n := strings.ToUpper(strings.TrimSpace(name))
	if !validName.MatchString(n) {
		return "", fmt.Errorf("%w: names must start with a letter and use only A-Z, 0-9 and _ (max 64)", ErrInvalid)
	}
	return n, nil
}

func (m *Manager) Put(ctx context.Context, ownerID, ownerName, name, value string) (string, error) {
	if !m.Enabled() {
		return "", ErrDisabled
	}
	n, err := NormalizeName(name)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%w: value is required", ErrInvalid)
	}
	if len(value) > maxValueBytes {
		return "", fmt.Errorf("%w: value is larger than %d bytes", ErrInvalid, maxValueBytes)
	}
	ct, err := m.box.Seal(ownerID, n, value)
	if err != nil {
		return "", err
	}
	return n, m.store.PutSecret(ctx, ownerID, ownerName, n, ct)
}

func (m *Manager) Delete(ctx context.Context, ownerID, name string) error {
	n, err := NormalizeName(name)
	if err != nil {
		return err
	}
	return m.store.DeleteSecret(ctx, ownerID, n)
}

// List returns the owner's secret names and which of their rules use them.
func (m *Manager) List(ctx context.Context, ownerID string) ([]store.SecretMeta, error) {
	list, err := m.store.ListSecrets(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	all, err := m.store.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.UpdatedBy != ownerID {
			continue
		}
		refs, err := actions.SecretRefs(r.Actions)
		if err != nil {
			continue
		}
		for i := range list {
			if slices.Contains(refs, list[i].Name) {
				list[i].UsedBy = append(list[i].UsedBy, r.Name)
			}
		}
	}
	reqs, err := m.store.ListRequests(ctx)
	if err != nil {
		return nil, err
	}
	for _, sr := range reqs {
		if sr.UpdatedBy != ownerID {
			continue
		}
		refs, err := actions.SecretRefs([]rules.Action{sr.Request})
		if err != nil {
			continue
		}
		for i := range list {
			if slices.Contains(refs, list[i].Name) {
				list[i].UsedBy = append(list[i].UsedBy, "Request: "+sr.Name)
			}
		}
	}
	return list, nil
}

// Available reports whether ownerID may use name: their own stored secret or
// an operator-provided Y6IFTTT_SECRET_* env var.
func (m *Manager) Available(ctx context.Context, ownerID, name string) (bool, error) {
	if ownerID != "" {
		ok, err := m.store.HasSecret(ctx, ownerID, name)
		if err != nil || ok {
			return ok, err
		}
	}
	_, ok := actions.EnvSecret(name)
	return ok, nil
}

// LookupFor resolves secrets for a run of a rule last saved by ownerID.
// The owner's stored secrets win over env secrets with the same name.
func (m *Manager) LookupFor(ctx context.Context, ownerID string) actions.SecretLookup {
	return func(name string) (string, error) {
		if ownerID != "" && m.Enabled() {
			ct, err := m.store.SecretCiphertext(ctx, ownerID, name)
			switch {
			case err == nil:
				return m.box.Open(ownerID, name, ct)
			case !errors.Is(err, store.ErrNotFound):
				return "", err
			}
		}
		if v, ok := actions.EnvSecret(name); ok {
			return v, nil
		}
		return "", fmt.Errorf("secret %q is not available to this rule (it runs with the secrets of whoever last saved it)", name)
	}
}

// CheckRule verifies every secret the actions reference is usable by
// editorID, so a rule can't be saved pointing at someone else's secret.
func (m *Manager) CheckRule(ctx context.Context, editorID string, acts []rules.Action) error {
	if err := actions.ParamSecretRefs(acts); err != nil {
		return err
	}
	refs, err := actions.SecretRefs(acts)
	if err != nil {
		return err
	}
	var missing []string
	for _, name := range refs {
		ok, err := m.Available(ctx, editorID, name)
		if err != nil {
			return err
		}
		if !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("unknown secret(s) %s: add them on the Secrets tab first (rules can only use your own secrets)", strings.Join(missing, ", "))
	}
	return nil
}
