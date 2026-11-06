// Package store is the SQLite persistence layer.
//
// Cookie values are stored encrypted (value_enc + nonce); the caller is
// responsible for encryption via the crypto.Vault. Metadata (label,
// domain, path...) stays plaintext so snapshots can be searched and
// paginated efficiently.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS profiles (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  note       TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS snapshots (
  id            TEXT PRIMARY KEY,
  profile_id    TEXT REFERENCES profiles(id) ON DELETE SET NULL,
  domain        TEXT NOT NULL,
  label         TEXT NOT NULL,
  note          TEXT NOT NULL DEFAULT '',
  source_url    TEXT NOT NULL DEFAULT '',
  store_id      TEXT NOT NULL DEFAULT '',
  is_replace    INTEGER NOT NULL DEFAULT 1,
  fingerprint   TEXT NOT NULL DEFAULT '',
  cookie_count  INTEGER NOT NULL DEFAULT 0,
  http_only_count INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS cookies (
  id            TEXT PRIMARY KEY,
  snapshot_id   TEXT NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  domain        TEXT NOT NULL,
  host_only     INTEGER NOT NULL DEFAULT 0,
  path          TEXT NOT NULL DEFAULT '/',
  secure        INTEGER NOT NULL DEFAULT 0,
  http_only     INTEGER NOT NULL DEFAULT 0,
  same_site     TEXT NOT NULL DEFAULT 'lax',
  expires_at    INTEGER,              -- unix seconds, NULL = session cookie
  partitioned   INTEGER NOT NULL DEFAULT 0,
  partition_key TEXT NOT NULL DEFAULT '',
  value_enc     BLOB NOT NULL,
  nonce         BLOB NOT NULL,
  hash          TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS tokens (
  id         TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  label      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_snap_domain_created
  ON snapshots(domain, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_snap_last_used
  ON snapshots(last_used_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_snap_created
  ON snapshots(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_snap_label
  ON snapshots(label);
CREATE INDEX IF NOT EXISTS idx_snap_profile
  ON snapshots(profile_id);
CREATE INDEX IF NOT EXISTS idx_cookies_snapshot
  ON cookies(snapshot_id, name);
CREATE INDEX IF NOT EXISTS idx_cookies_domain
  ON cookies(domain);

CREATE TABLE IF NOT EXISTS audit_log (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  ts          INTEGER NOT NULL,
  action      TEXT NOT NULL,
  snapshot_id TEXT NOT NULL DEFAULT '',
  detail      TEXT NOT NULL DEFAULT ''
);
`

// ftsSchema is FTS5 (trigram) full-text index over snapshots.
// It requires the go-sqlite3 driver built with -tags sqlite_fts5;
// if unavailable, Open falls back to LIKE search (ftsEnabled=false).
const ftsSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS snapshots_fts USING fts5(
  label, domain, note, content='snapshots', content_rowid='rowid',
  tokenize='trigram');
CREATE TRIGGER IF NOT EXISTS snapshots_fts_insert AFTER INSERT ON snapshots BEGIN
  INSERT INTO snapshots_fts(rowid, label, domain, note)
  VALUES (new.rowid, new.label, new.domain, new.note);
END;
CREATE TRIGGER IF NOT EXISTS snapshots_fts_delete AFTER DELETE ON snapshots BEGIN
  INSERT INTO snapshots_fts(snapshots_fts, rowid, label, domain, note)
  VALUES ('delete', old.rowid, old.label, old.domain, old.note);
END;
CREATE TRIGGER IF NOT EXISTS snapshots_fts_update AFTER UPDATE ON snapshots BEGIN
  INSERT INTO snapshots_fts(snapshots_fts, rowid, label, domain, note)
  VALUES ('delete', old.rowid, old.label, old.domain, old.note);
  INSERT INTO snapshots_fts(rowid, label, domain, note)
  VALUES (new.rowid, new.label, new.domain, new.note);
END;
`

// Store wraps *sql.DB.
type Store struct {
	DB         *sql.DB
	ftsEnabled bool
}

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?cache=shared&_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite single writer
	st := &Store{DB: db}
	if _, err := db.Exec(ftsSchema); err == nil {
		// Rebuild FTS index from content (covers DBs created before FTS).
		_, _ = db.Exec(`INSERT INTO snapshots_fts(snapshots_fts) VALUES('rebuild')`)
		st.ftsEnabled = true
	}
	return st, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

// --- meta ---------------------------------------------------------------

func (s *Store) MetaGet(key string) ([]byte, bool, error) {
	var v []byte
	err := s.DB.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return v, err == nil, err
}

func (s *Store) MetaSet(key string, value []byte) error {
	_, err := s.DB.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) Initialized() (bool, error) {
	v, ok, err := s.MetaGet("wrapped_dek")
	return ok && len(v) > 0, err
}

// --- tokens ---------------------------------------------------------------

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) TokenCreate(id, tokenHash, label string, now time.Time) error {
	_, err := s.DB.Exec(`INSERT INTO tokens(id,token_hash,created_at,label) VALUES(?,?,?,?)`,
		id, tokenHash, now.Unix(), label)
	return err
}

func (s *Store) TokenExists(tokenHash string) (bool, error) {
	var one int
	err := s.DB.QueryRow(`SELECT 1 FROM tokens WHERE token_hash=?`, tokenHash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// --- profiles ---------------------------------------------------------------

type Profile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func (s *Store) ProfileCreate(p Profile) error {
	_, err := s.DB.Exec(`INSERT INTO profiles(id,name,note,created_at,updated_at) VALUES(?,?,?,?,?)`,
		p.ID, p.Name, p.Note, p.CreatedAt, p.UpdatedAt)
	return err
}

func (s *Store) ProfileList() ([]Profile, error) {
	rows, err := s.DB.Query(`SELECT id,name,note,created_at,updated_at FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.ID, &p.Name, &p.Note, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if out == nil {
		out = []Profile{}
	}
	return out, rows.Err()
}

func (s *Store) ProfileByName(name string) (Profile, bool, error) {
	var p Profile
	err := s.DB.QueryRow(`SELECT id,name,note,created_at,updated_at FROM profiles WHERE name=?`, name).
		Scan(&p.ID, &p.Name, &p.Note, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	return p, err == nil, err
}

func (s *Store) ProfileDelete(id string) error {
	_, err := s.DB.Exec(`DELETE FROM profiles WHERE id=?`, id)
	return err
}

// --- snapshots ---------------------------------------------------------------

type Snapshot struct {
	ID             string `json:"id"`
	ProfileID      string `json:"profile_id"`
	Domain         string `json:"domain"`
	Label          string `json:"label"`
	Note           string `json:"note"`
	SourceURL      string `json:"source_url"`
	StoreID        string `json:"store_id"`
	IsReplace      bool   `json:"is_replace"`
	Fingerprint    string `json:"fingerprint"`
	CookieCount    int    `json:"cookie_count"`
	HTTPOnlyCount  int    `json:"http_only_count"`
	CreatedAt      int64  `json:"created_at"`
	LastUsedAt     int64  `json:"last_used_at"`
}

// Fingerprint hashes cookie metadata+values so identical re-saves can be
// detected / warned about (cross-snapshot storage is still kept).
func Fingerprint(parts []string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// CookieInput is a decrypted cookie as submitted by the extension.
type CookieInput struct {
	Name          string          `json:"name"`
	Value         string          `json:"value"`
	Domain        string          `json:"domain"`
	HostOnly      bool            `json:"hostOnly"`
	Path          string          `json:"path"`
	Secure        bool            `json:"secure"`
	HTTPOnly      bool            `json:"httpOnly"`
	SameSite      string          `json:"sameSite"`
	ExpirationDate *float64       `json:"expirationDate,omitempty"`
	PartitionKey  string          `json:"partitionKey,omitempty"` // JSON string form
	Partitioned   bool            `json:"partitioned,omitempty"`
}

// EncryptedCookie is a cookie ready for storage.
type EncryptedCookie struct {
	Input      CookieInput
	ValueEnc   []byte
	Nonce      []byte
	Hash       string
	ExpiresAt  *int64
}

func (s *Store) SnapshotCreate(snap Snapshot, cookies []EncryptedCookie) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO snapshots
		(id,profile_id,domain,label,note,source_url,store_id,is_replace,fingerprint,cookie_count,http_only_count,created_at,last_used_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		snap.ID, nullStr(snap.ProfileID), snap.Domain, snap.Label, snap.Note,
		snap.SourceURL, snap.StoreID, boolInt(snap.IsReplace), snap.Fingerprint,
		snap.CookieCount, snap.HTTPOnlyCount, snap.CreatedAt, snap.LastUsedAt); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO cookies
		(id,snapshot_id,name,domain,host_only,path,secure,http_only,same_site,expires_at,partitioned,partition_key,value_enc,nonce,hash)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, c := range cookies {
		if _, err := stmt.Exec(
			fmt.Sprintf("%s_%d", snap.ID, i), snap.ID, c.Input.Name, c.Input.Domain,
			boolInt(c.Input.HostOnly), orDefault(c.Input.Path, "/"),
			boolInt(c.Input.Secure), boolInt(c.Input.HTTPOnly),
			orDefault(strings.ToLower(c.Input.SameSite), "lax"),
			c.ExpiresAt, boolInt(c.Input.Partitioned), c.Input.PartitionKey,
			c.ValueEnc, c.Nonce, c.Hash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- listing with cursor pagination -----------------------------------------

type ListQuery struct {
	Q         string
	Domain    string
	ProfileID string
	Sort      string // last_used_at:desc | created_at:desc | created_at:asc | label:asc
	Limit     int
	// CursorKey is the sort-key value (unix millis, or label) of the last
	// item of the previous page; empty = first page.
	CursorKey string
	CursorID  string
}

type ListResult struct {
	Items      []Snapshot
	NextCursor string // opaque; empty when has_more=false
	HasMore    bool
	Total      int
}

// ftsQuery builds an FTS5 trigram query from free text: each token with
// at least 3 runes becomes a quoted prefix term, ANDed together.
// Returns ok=false when no usable token exists (caller falls back to LIKE,
// which also covers short / CJK queries trigram cannot match).
func ftsQuery(q string) (string, bool) {
	var terms []string
	for _, tok := range strings.Fields(q) {
		tok = strings.Trim(tok, "\"'")
		if len([]rune(tok)) < 3 {
			continue
		}
		terms = append(terms, `"`+tok+`"*`)
	}
	if len(terms) == 0 {
		return "", false
	}
	return strings.Join(terms, " AND "), true
}

func sortColumn(sort string) (col, dir string, ok bool) {
	switch sort {
	case "", "last_used_at:desc":
		return "last_used_at", "DESC", true
	case "created_at:desc":
		return "created_at", "DESC", true
	case "created_at:asc":
		return "created_at", "ASC", true
	case "label:asc":
		return "label", "ASC", true
	}
	return "", "", false
}

func (q ListQuery) normalized() (col, dir string, limit int) {
	col, dir, ok := sortColumn(q.Sort)
	if !ok {
		col, dir = "last_used_at", "DESC"
	}
	limit = q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return col, dir, limit
}

func (s *Store) SnapshotCount(q ListQuery) (int, error) {
	where, args := s.listWhere(q)
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM snapshots`+where, args...).Scan(&n)
	return n, err
}

func (s *Store) listWhere(q ListQuery) (string, []any) {
	var clauses []string
	var args []any
	if q.Domain != "" {
		clauses = append(clauses, "domain LIKE ?")
		args = append(args, "%"+q.Domain+"%")
	}
	if q.ProfileID != "" {
		clauses = append(clauses, "profile_id = ?")
		args = append(args, q.ProfileID)
	}
	if q.Q != "" {
		usedFTS := false
		if s.ftsEnabled {
			if fts, ok := ftsQuery(q.Q); ok {
				clauses = append(clauses, "EXISTS (SELECT 1 FROM snapshots_fts WHERE snapshots_fts.rowid = snapshots.rowid AND snapshots_fts MATCH ?)")
				args = append(args, fts)
				usedFTS = true
			}
		}
		if !usedFTS {
			// LIKE fallback (no FTS, or short/CJK query trigram can't match).
			clauses = append(clauses, "(label LIKE ? OR domain LIKE ? OR note LIKE ?)")
			like := "%" + q.Q + "%"
			args = append(args, like, like, like)
		}
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func (s *Store) SnapshotList(q ListQuery) (ListResult, error) {
	col, dir, limit := q.normalized()
	where, args := s.listWhere(q)

	if q.CursorKey != "" && q.CursorID != "" {
		// Keyset condition: (col, id) strictly after cursor in sort order.
		var op string
		if dir == "DESC" {
			op = "<"
		} else {
			op = ">"
		}
		cond := fmt.Sprintf("(%s %s ? OR (%s = ? AND id %s ?))", col, op, col, tieOp(dir))
		if where == "" {
			where = " WHERE " + cond
		} else {
			where += " AND " + cond
		}
		var keyVal any = q.CursorKey
		if col != "label" {
			var n int64
			fmt.Sscan(q.CursorKey, &n)
			keyVal = n
		}
		args = append(args, keyVal, keyVal, q.CursorID)
	}

	query := fmt.Sprintf(`SELECT id,profile_id,domain,label,note,source_url,store_id,is_replace,
		fingerprint,cookie_count,http_only_count,created_at,last_used_at
		FROM snapshots%s ORDER BY %s %s, id %s LIMIT ?`, where, col, dir, tieDir(dir))
	args = append(args, limit+1)

	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return ListResult{}, err
	}
	defer rows.Close()

	var items []Snapshot
	for rows.Next() {
		var snap Snapshot
		var profileID sql.NullString
		var isReplace int
		if err := rows.Scan(&snap.ID, &profileID, &snap.Domain, &snap.Label, &snap.Note,
			&snap.SourceURL, &snap.StoreID, &isReplace, &snap.Fingerprint,
			&snap.CookieCount, &snap.HTTPOnlyCount, &snap.CreatedAt, &snap.LastUsedAt); err != nil {
			return ListResult{}, err
		}
		snap.ProfileID = profileID.String
		snap.IsReplace = isReplace != 0
		items = append(items, snap)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, err
	}
	if items == nil {
		items = []Snapshot{}
	}

	res := ListResult{Items: items}
	if len(items) > limit {
		res.HasMore = true
		res.Items = items[:limit]
		last := res.Items[len(res.Items)-1]
		res.NextCursor = encodeCursor(last, col)
	}
	return res, nil
}

func tieOp(dir string) string {
	if dir == "DESC" {
		return "<"
	}
	return ">"
}
func tieDir(dir string) string {
	if dir == "DESC" {
		return "DESC"
	}
	return "ASC"
}

func encodeCursor(snap Snapshot, col string) string {
	var key string
	switch col {
	case "label":
		key = snap.Label
	case "created_at":
		key = fmt.Sprintf("%d", snap.CreatedAt)
	default:
		key = fmt.Sprintf("%d", snap.LastUsedAt)
	}
	// Opaque cursor: base64url("<key>\n<id>") implemented in api package;
	// here we simply join — api layer owns the encoding.
	return key + "\n" + snap.ID
}

// DecodeCursor splits a cursor produced by encodeCursor.
func DecodeCursor(cursor string) (key, id string, ok bool) {
	parts := strings.SplitN(cursor, "\n", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Store) SnapshotGet(id string) (Snapshot, bool, error) {
	var snap Snapshot
	var profileID sql.NullString
	var isReplace int
	err := s.DB.QueryRow(`SELECT id,profile_id,domain,label,note,source_url,store_id,is_replace,
		fingerprint,cookie_count,http_only_count,created_at,last_used_at
		FROM snapshots WHERE id=?`, id).Scan(
		&snap.ID, &profileID, &snap.Domain, &snap.Label, &snap.Note,
		&snap.SourceURL, &snap.StoreID, &isReplace, &snap.Fingerprint,
		&snap.CookieCount, &snap.HTTPOnlyCount, &snap.CreatedAt, &snap.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	snap.ProfileID = profileID.String
	snap.IsReplace = isReplace != 0
	return snap, true, nil
}

// GroupCount is a main-domain group with snapshot count.
type GroupCount struct {
	MainDomain string `json:"main_domain"`
	Count      int    `json:"count"`
}

// SnapshotGroups aggregates snapshots by (Go-computed) main domain.
// The aggregation itself is done in the api layer via DomainMain;
// this returns raw domain counts.
func (s *Store) SnapshotDomainCounts() ([]GroupCount, error) {
	rows, err := s.DB.Query(`SELECT domain, COUNT(*) FROM snapshots GROUP BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupCount
	for rows.Next() {
		var g GroupCount
		if err := rows.Scan(&g.MainDomain, &g.Count); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SnapshotAll returns every snapshot (used for backup export).
func (s *Store) SnapshotAll() ([]Snapshot, error) {
	res, err := s.SnapshotList(ListQuery{Limit: 200, Sort: "created_at:asc"})
	if err != nil {
		return nil, err
	}
	out := res.Items
	for res.HasMore {
		key, id, _ := DecodeCursor(res.NextCursor)
		res, err = s.SnapshotList(ListQuery{Limit: 200, Sort: "created_at:asc", CursorKey: key, CursorID: id})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
	}
	return out, nil
}

// SnapshotExistsFingerprint reports whether an identical snapshot
// (same domain+label+fingerprint) already exists (backup import dedup).
func (s *Store) SnapshotExistsFingerprint(domain, label, fingerprint string) (bool, error) {
	var one int
	err := s.DB.QueryRow(`SELECT 1 FROM snapshots WHERE domain=? AND label=? AND fingerprint=? LIMIT 1`,
		domain, label, fingerprint).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) SnapshotSetReplace(id string, isReplace bool) error {
	_, err := s.DB.Exec(`UPDATE snapshots SET is_replace=? WHERE id=?`, boolInt(isReplace), id)
	return err
}

func (s *Store) SnapshotUpdate(id, label, note string) error {
	_, err := s.DB.Exec(`UPDATE snapshots SET label=?, note=? WHERE id=?`, label, note, id)
	return err
}

func (s *Store) SnapshotTouch(id string, now time.Time) error {
	_, err := s.DB.Exec(`UPDATE snapshots SET last_used_at=? WHERE id=?`, now.Unix(), id)
	return err
}

func (s *Store) SnapshotDelete(id string) error {
	_, err := s.DB.Exec(`DELETE FROM snapshots WHERE id=?`, id)
	return err
}

// StoredCookie is an encrypted cookie row.
type StoredCookie struct {
	ID           string
	Name         string
	Domain       string
	HostOnly     bool
	Path         string
	Secure       bool
	HTTPOnly     bool
	SameSite     string
	ExpiresAt    *int64
	Partitioned  bool
	PartitionKey string
	ValueEnc     []byte
	Nonce        []byte
}

func (s *Store) SnapshotCookies(id string) ([]StoredCookie, error) {
	rows, err := s.DB.Query(`SELECT id,name,domain,host_only,path,secure,http_only,same_site,
		expires_at,partitioned,partition_key,value_enc,nonce
		FROM cookies WHERE snapshot_id=? ORDER BY name, path`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredCookie
	for rows.Next() {
		var c StoredCookie
		var hostOnly, secure, httpOnly, partitioned int
		var expires sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Name, &c.Domain, &hostOnly, &c.Path, &secure,
			&httpOnly, &c.SameSite, &expires, &partitioned, &c.PartitionKey,
			&c.ValueEnc, &c.Nonce); err != nil {
			return nil, err
		}
		c.HostOnly, c.Secure, c.HTTPOnly, c.Partitioned =
			hostOnly != 0, secure != 0, httpOnly != 0, partitioned != 0
		if expires.Valid {
			v := expires.Int64
			c.ExpiresAt = &v
		}
		out = append(out, c)
	}
	if out == nil {
		out = []StoredCookie{}
	}
	return out, rows.Err()
}

// --- audit ------------------------------------------------------------------

func (s *Store) Audit(action, snapshotID, detail string, now time.Time) {
	// Never log cookie values: detail must be metadata only.
	_, _ = s.DB.Exec(`INSERT INTO audit_log(ts,action,snapshot_id,detail) VALUES(?,?,?,?)`,
		now.Unix(), action, snapshotID, detail)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
