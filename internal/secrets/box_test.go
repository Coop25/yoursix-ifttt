package secrets

import (
	"bytes"
	"testing"

	"yoursix-ifttt/internal/actions"
)

func TestBoxBindsOwnerAndName(t *testing.T) {
	b, err := NewBox(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := b.Seal("alice", "CAM_PASS", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := b.Open("alice", "CAM_PASS", ct); err != nil || v != "s3cret" {
		t.Fatalf("round trip: %q %v", v, err)
	}
	// Copying alice's row to bob, or renaming it, must not decrypt.
	if _, err := b.Open("bob", "CAM_PASS", ct); err == nil {
		t.Fatal("ciphertext usable under another owner")
	}
	if _, err := b.Open("alice", "OTHER", ct); err == nil {
		t.Fatal("ciphertext usable under another name")
	}
	other, _ := NewBox(bytes.Repeat([]byte{2}, 32))
	if _, err := other.Open("alice", "CAM_PASS", ct); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

func TestParseKey(t *testing.T) {
	if _, err := ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKey("too-short"); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestSecretNamesRequireLiterals(t *testing.T) {
	names, err := actions.SecretNames(`{{ if .data.x }}{{ secret "a_pass" }}{{ end }}:{{ secret "B" | urlquery }}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "A_PASS" || names[1] != "B" {
		t.Fatalf("names = %v", names)
	}
	for _, bad := range []string{
		`{{ secret .data.deviceName }}`, // dynamic name could probe other secrets
		`{{ "X" | secret }}`,
		`{{ printf "%s" (secret .rule) }}`,
	} {
		if _, err := actions.SecretNames(bad); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}
