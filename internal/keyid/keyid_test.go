package keyid

import "testing"

func TestFingerprint(t *testing.T) {
	id, hint := Fingerprint("sk-proj-abcdefx9a1")
	if len(id) != 14 || id[:2] != "k_" {
		t.Fatalf("unexpected id %q", id)
	}
	if hint != "…x9a1" {
		t.Fatalf("unexpected hint %q", hint)
	}
	id2, _ := Fingerprint("sk-proj-abcdefx9a1")
	if id != id2 {
		t.Fatal("fingerprint is not stable")
	}
	if other, _ := Fingerprint("sk-proj-other"); other == id {
		t.Fatal("different keys share an id")
	}
	if id, hint := Fingerprint(""); id != "" || hint != "" {
		t.Fatal("empty key must yield empty fingerprint")
	}
	if _, hint := Fingerprint("abcd"); hint != "" {
		t.Fatal("short keys must not expose a hint")
	}
}
