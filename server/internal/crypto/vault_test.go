package crypto

import "testing"

func TestWrapUnwrapAndEncrypt(t *testing.T) {
	wrapped, dek, err := NewWrappedDEK("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapDEK("wrong password", wrapped); err == nil {
		t.Fatal("expected wrong password error")
	}
	got, err := UnwrapDEK("correct horse battery", wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(dek) {
		t.Fatal("dek mismatch")
	}

	v := &Vault{}
	if _, _, err := v.Encrypt("secret"); err == nil {
		t.Fatal("expected locked error")
	}
	v.Unlock(dek)
	nonce, ct, err := v.Encrypt("session=abc123")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := v.Decrypt(nonce, ct)
	if err != nil || pt != "session=abc123" {
		t.Fatalf("roundtrip failed: %q %v", pt, err)
	}
	v.Lock()
	if v.IsUnlocked() {
		t.Fatal("expected locked")
	}
	if _, err := v.Decrypt(nonce, ct); err == nil {
		t.Fatal("expected locked decrypt error")
	}
}

func TestRewrap(t *testing.T) {
	_, dek, _ := NewWrappedDEK("old-password-1")
	rewrapped, err := Rewrap(dek, "new-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapDEK("new-password-1", rewrapped); err != nil {
		t.Fatal(err)
	}
}
