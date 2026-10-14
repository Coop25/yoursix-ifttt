package actions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
)

// SecretEnvPrefix scopes which environment variables templates may read, so a
// rule editor can reference operator-provided secrets without being able to
// read the session secret or OAuth credentials.
const SecretEnvPrefix = "Y6IFTTT_SECRET_"

// SecretLookup resolves {{ secret "NAME" }} for one rule run. The engine
// supplies one scoped to the rule's owner; nil means env secrets only.
type SecretLookup func(name string) (string, error)

// EnvSecret reads a Y6IFTTT_SECRET_* variable.
func EnvSecret(name string) (string, bool) {
	return os.LookupEnv(SecretEnvPrefix + strings.ToUpper(name))
}

func envOnly(name string) (string, error) {
	if v, ok := EnvSecret(name); ok {
		return v, nil
	}
	return "", fmt.Errorf("secret %q is not set", name)
}

func funcMap(lookup SecretLookup) template.FuncMap {
	if lookup == nil {
		lookup = envOnly
	}
	return template.FuncMap{
		// {{ secret "FRONT_CAM_PASS" }}
		"secret": func(name string) (string, error) { return lookup(strings.ToUpper(strings.TrimSpace(name))) },
		// {{ json .data.deviceName }} -> "CAM-01" (quoted, escaped) for JSON bodies.
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
		"urlquery": url.QueryEscape,
		"lower":    strings.ToLower,
		"upper":    strings.ToUpper,
		"default": func(def, v any) any {
			if v == nil || v == "" {
				return def
			}
			return v
		},
	}
}

// Render executes s as a text/template against data. Plain strings without
// "{{" are returned untouched.
func Render(s string, data map[string]any, lookup SecretLookup) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	t, err := template.New("").Funcs(funcMap(lookup)).Option("missingkey=zero").Parse(s)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func parseTemplate(s string) (*template.Template, error) {
	return template.New("").Funcs(funcMap(nil)).Parse(s)
}

// CheckTemplate parses s so editors get syntax errors at save time.
func CheckTemplate(s string) error {
	if !strings.Contains(s, "{{") {
		return nil
	}
	_, err := parseTemplate(s)
	return err
}

// SecretNames returns the (upper-cased) names passed as string literals to
// `secret` in s. A `secret` call with a non-literal argument is an error, so
// save-time checks can't be bypassed with {{ secret .data.x }}.
func SecretNames(s string) ([]string, error) {
	if !strings.Contains(s, "{{") {
		return nil, nil
	}
	t, err := parseTemplate(s)
	if err != nil {
		return nil, err
	}
	found := map[string]struct{}{}
	var walkErr error
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		if n == nil || walkErr != nil {
			return
		}
		switch n := n.(type) {
		case *parse.ListNode:
			if n == nil {
				return
			}
			for _, c := range n.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walk(n.Pipe)
		case *parse.IfNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.RangeNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.WithNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.TemplateNode:
			walk(n.Pipe)
		case *parse.PipeNode:
			if n == nil {
				return
			}
			for _, c := range n.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for i, a := range n.Args {
				if id, ok := a.(*parse.IdentifierNode); ok && id.Ident == "secret" {
					if i != 0 || len(n.Args) != 2 {
						walkErr = fmt.Errorf("secret must be called as {{ secret \"NAME\" }}")
						return
					}
					str, ok := n.Args[1].(*parse.StringNode)
					if !ok {
						walkErr = fmt.Errorf("secret name must be a quoted literal, e.g. {{ secret \"CAMERA_PASS\" }}")
						return
					}
					found[strings.ToUpper(strings.TrimSpace(str.Text))] = struct{}{}
				}
				walk(a)
			}
		case *parse.ChainNode:
			walk(n.Node)
		}
	}
	walk(t.Tree.Root)
	if walkErr != nil {
		return nil, walkErr
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}
