package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSnapshotCreateListSearchPagination(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()

	// Create 5 snapshots for the same domain (different accounts).
	for i := 0; i < 5; i++ {
		label := []string{"alice", "bob", "carol", "dave", "erin"}[i]
		snap := Snapshot{
			ID: "s_" + label, Domain: "example.com", Label: label + "-account",
			CookieCount: 1, CreatedAt: now + int64(i)*1000, LastUsedAt: now + int64(i)*1000,
		}
		ck := []EncryptedCookie{{
			Input:    CookieInput{Name: "sid", Value: "v" + label, Domain: ".example.com"},
			ValueEnc: []byte("enc"), Nonce: []byte("n"),
		}}
		if err := st.SnapshotCreate(snap, ck); err != nil {
			t.Fatal(err)
		}
	}

	// Search.
	res, err := st.SnapshotList(ListQuery{Q: "bob", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Label != "bob-account" {
		t.Fatalf("search failed: %+v", res.Items)
	}

	// Cursor pagination: page size 2, walk all.
	var seen []string
	var cursorKey, cursorID string
	for {
		res, err := st.SnapshotList(ListQuery{Limit: 2, CursorKey: cursorKey, CursorID: cursorID})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range res.Items {
			seen = append(seen, it.Label)
		}
		if !res.HasMore {
			break
		}
		cursorKey, cursorID, _ = DecodeCursor(res.NextCursor)
		if cursorID == "" {
			t.Fatal("missing cursor")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("pagination expected 5, got %d: %v", len(seen), seen)
	}
	// Default sort is last_used_at DESC: erin first.
	if seen[0] != "erin-account" {
		t.Fatalf("expected erin first, got %v", seen)
	}

	// Cookies roundtrip (encrypted blob untouched by store).
	cks, err := st.SnapshotCookies("s_bob")
	if err != nil || len(cks) != 1 || string(cks[0].ValueEnc) != "enc" {
		t.Fatalf("cookies: %v %+v", err, cks)
	}

	// Delete cascades.
	if err := st.SnapshotDelete("s_bob"); err != nil {
		t.Fatal(err)
	}
	cks, _ = st.SnapshotCookies("s_bob")
	if len(cks) != 0 {
		t.Fatal("expected cascade delete")
	}
}
