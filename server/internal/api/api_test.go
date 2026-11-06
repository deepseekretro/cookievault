package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	vaultcrypto "cookievault/server/internal/crypto"
	"cookievault/server/internal/store"
)

func setupServer(t *testing.T) (*Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v := &vaultcrypto.Vault{}
	return New(st, v, "12345678"), filepath.Join(t.TempDir(), "x")
}

func doReq(t *testing.T, srv *Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Host = "127.0.0.1:18765"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestCompareAndSingleExport(t *testing.T) {
	srv, _ := setupServer(t)
	code, setup := doReq(t, srv, "POST", "/v1/auth/setup", "", map[string]any{"password": "master-password-1"})
	if code != 201 {
		t.Fatalf("setup: %d %v", code, setup)
	}
	token := setup["token"].(string)

	mkSnap := func(label string, cookies []map[string]any) map[string]any {
		code, out := doReq(t, srv, "POST", "/v1/snapshots", token, map[string]any{
			"domain": "example.com", "label": label, "cookies": cookies,
		})
		if code != 201 {
			t.Fatalf("create %s: %d %v", label, code, out)
		}
		return out
	}
	mkCookie := func(name, val string) map[string]any {
		return map[string]any{"name": name, "value": val, "domain": ".example.com", "hostOnly": false, "path": "/", "secure": true, "httpOnly": true, "sameSite": "lax"}
	}
	a := mkSnap("alice", []map[string]any{mkCookie("sid", "alice-1"), mkCookie("theme", "dark"), mkCookie("only_a", "x")})
	b := mkSnap("bob", []map[string]any{mkCookie("sid", "bob-2"), mkCookie("theme", "dark"), mkCookie("only_b", "y")})

	code, cmp := doReq(t, srv, "GET", "/v1/snapshots/compare?a="+a["id"].(string)+"&b="+b["id"].(string), token, nil)
	if code != 200 {
		t.Fatalf("compare: %d %v", code, cmp)
	}
	if len(cmp["added"].([]any)) != 1 || len(cmp["removed"].([]any)) != 1 || len(cmp["changed"].([]any)) != 1 || cmp["unchanged"].(float64) != 1 {
		t.Fatalf("unexpected diff: %+v", cmp)
	}
	// Compare must not leak values.
	raw, _ := json.Marshal(cmp)
	if bytes.Contains(raw, []byte("alice-1")) || bytes.Contains(raw, []byte("bob-2")) {
		t.Fatal("compare leaked cookie values")
	}

	// Single snapshot export uses the same encrypted format and imports.
	code, exp := doReq(t, srv, "POST", "/v1/snapshots/"+a["id"].(string)+"/export", token, map[string]any{"backup_password": "backup-password-1"})
	if code != 200 {
		t.Fatalf("export: %d %v", code, exp)
	}
	// Verify the payload really decrypts to one snapshot.
	kdfRaw, _ := json.Marshal(exp["kdf"])
	var pe vaultcrypto.PasswordEncrypted
	_ = json.Unmarshal(kdfRaw, &pe)
	plain, err := vaultcrypto.DecryptWithPassword("backup-password-1", &pe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain, []byte("alice")) {
		t.Fatalf("export payload unexpected: %s", plain)
	}

	// Profile grouping: create profile, snapshot under it, filter.
	code, prof := doReq(t, srv, "POST", "/v1/profiles", token, map[string]any{"name": "work"})
	if code != 201 {
		t.Fatalf("profile: %d %v", code, prof)
	}
	code, _ = doReq(t, srv, "POST", "/v1/snapshots", token, map[string]any{
		"profile_id": prof["id"].(string), "domain": "example.com", "label": "carol", "cookies": []map[string]any{mkCookie("sid", "c")},
	})
	if code != 201 {
		t.Fatalf("profile snapshot: %d", code)
	}
	code, list := doReq(t, srv, "GET", "/v1/snapshots?profile_id="+prof["id"].(string), token, nil)
	if code != 200 || list["total"].(float64) != 1 {
		t.Fatalf("profile filter: %d %+v", code, list)
	}
	if list["items"].([]any)[0].(map[string]any)["label"] != "carol" {
		t.Fatal("profile filter wrong item")
	}
}
