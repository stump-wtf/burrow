package seal

import (
	"errors"
	"strings"
	"testing"
)

func TestSealRoundTrip(t *testing.T) {
	sealer, err := New("a-deployment-credential-key")
	if err != nil {
		t.Fatal(err)
	}
	if !sealer.Configured() {
		t.Fatal("a configured key must give a configured sealer")
	}
	sealed, err := sealer.Seal("sk-upstream-secret")
	if err != nil {
		t.Fatal(err)
	}
	if sealed == "" || strings.Contains(sealed, "secret") {
		t.Fatalf("sealed value must not carry the plaintext: %q", sealed)
	}
	opened, err := sealer.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened != "sk-upstream-secret" {
		t.Fatalf("Open = %q", opened)
	}
	// Sealing the same value twice gives two ciphertexts: a database read cannot
	// group rows by credential just because they share a key.
	again, err := sealer.Seal("sk-upstream-secret")
	if err != nil {
		t.Fatal(err)
	}
	if again == sealed {
		t.Fatal("seal must use a fresh nonce")
	}
}

func TestSealEmptyIsNothingToStore(t *testing.T) {
	sealer, err := New("a-deployment-credential-key")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal("")
	if err != nil {
		t.Fatal(err)
	}
	if sealed != "" {
		t.Fatalf("an empty credential must store nothing, got %q", sealed)
	}
	opened, err := sealer.Open("")
	if err != nil || opened != "" {
		t.Fatalf("Open(\"\") = %q, %v", opened, err)
	}
}

func TestSealerWithoutKeyRefuses(t *testing.T) {
	sealer, err := New("")
	if err != nil {
		t.Fatalf("no key is a supported configuration, not an error: %v", err)
	}
	if sealer.Configured() {
		t.Fatal("an empty key must not report itself configured")
	}
	if _, err := sealer.Seal("sk-upstream-secret"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Seal without a key = %v, want ErrNotConfigured", err)
	}
	if _, err := sealer.Open("whatever"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Open without a key = %v, want ErrNotConfigured", err)
	}
	var nilSealer *Sealer
	if _, err := nilSealer.Seal("x"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("a nil sealer must refuse: %v", err)
	}
}

func TestOpenRefusesAnotherKeyAndTampering(t *testing.T) {
	first, err := New("key-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New("key-two")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := first.Seal("sk-upstream-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Open(sealed); err == nil {
		t.Fatal("a credential sealed under another key must not open")
	}
	tampered := sealed[:len(sealed)-2] + "ab"
	if tampered == sealed {
		t.Skip("nothing to tamper with")
	}
	if _, err := first.Open(tampered); err == nil {
		t.Fatal("a tampered credential must not open")
	}
	if _, err := first.Open("not base64 !!"); err == nil {
		t.Fatal("undecodable input must be an error, not a plaintext")
	}
}

func TestFingerprintIsStableAndCarriesNothing(t *testing.T) {
	first := Fingerprint("sk-upstream-secret")
	if first != Fingerprint("sk-upstream-secret") {
		t.Fatal("a fingerprint must identify the same credential twice")
	}
	if first == Fingerprint("sk-something-else") {
		t.Fatal("different credentials must not share a fingerprint")
	}
	if len(first) != 64 || strings.Contains(first, "secret") {
		t.Fatalf("fingerprint = %q", first)
	}
}
