// Package api implements the HTTP API of CookieVault.
//
// Security model (P1):
//   - Bind 127.0.0.1 only.
//   - Host must be loopback; Origin (when present) must be a browser
//     extension origin or loopback — this blocks DNS-rebinding / CSRF
//     attempts from ordinary web pages.
//   - Data endpoints require a Bearer token obtained via pairing.
//   - Data endpoints also require the vault to be unlocked.
package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	vaultcrypto "cookievault/server/internal/crypto"
	"cookievault/server/internal/store"
)

const version = "0.1.0"

// Server holds dependencies.
type Server struct {
	Store   *store.Store
	Vault   *vaultcrypto.Vault
	PairCode string
	PairExp  time.Time
	// AllowedHosts are extra Host values accepted besides loopback,
	// for deployments behind a reverse proxy (e.g. OpenResty at
	// https://edge02.10w123.com/cookievault/). Empty = loopback only.
	AllowedHosts map[string]bool

	mu          sync.Mutex
	unlockFails int
	lockedUntil time.Time

	lastActivity atomic.Int64 // unix seconds of last authenticated request
}

// TouchActivity records user activity (for idle auto-lock).
func (s *Server) TouchActivity() { s.lastActivity.Store(time.Now().Unix()) }

// LastActivity returns unix seconds of last activity (0 = none yet).
func (s *Server) LastActivity() int64 { return s.lastActivity.Load() }

// New creates a Server. pairCode is printed to the log at startup.
func New(st *store.Store, v *vaultcrypto.Vault, pairCode string) *Server {
	return &Server{Store: st, Vault: v, PairCode: pairCode, PairExp: time.Now().Add(5 * time.Minute), AllowedHosts: map[string]bool{}}
}

// SetAllowedHosts sets extra accepted Host values (comma-separated).
func (s *Server) SetAllowedHosts(list string) {
	for _, h := range strings.Split(list, ",") {
		h = strings.TrimSpace(strings.ToLower(h))
		if h != "" {
			s.AllowedHosts[h] = true
		}
	}
}

// Handler returns the root handler with guards applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/auth/status", s.authStatus)
	mux.HandleFunc("POST /v1/auth/setup", s.authSetup)
	mux.HandleFunc("POST /v1/auth/unlock", s.authUnlock)
	mux.HandleFunc("POST /v1/auth/lock", s.requireToken(s.authLock))
	mux.HandleFunc("POST /v1/auth/change-password", s.requireToken(s.changePassword))
	mux.HandleFunc("POST /v1/pair/verify", s.pairVerify)
	mux.HandleFunc("POST /v1/pair/code", s.requireToken(s.pairCode))
	mux.HandleFunc("GET /v1/profiles", s.requireToken(s.requireUnlocked(s.profileList)))
	mux.HandleFunc("POST /v1/profiles", s.requireToken(s.requireUnlocked(s.profileCreate)))
	mux.HandleFunc("DELETE /v1/profiles/{id}", s.requireToken(s.requireUnlocked(s.profileDelete)))
	mux.HandleFunc("POST /v1/snapshots", s.requireToken(s.requireUnlocked(s.snapshotCreate)))
	mux.HandleFunc("GET /v1/snapshots", s.requireToken(s.requireUnlocked(s.snapshotList)))
	mux.HandleFunc("GET /v1/snapshots/groups", s.requireToken(s.requireUnlocked(s.snapshotGroups)))
	mux.HandleFunc("GET /v1/snapshots/compare", s.requireToken(s.requireUnlocked(s.snapshotCompare)))
	mux.HandleFunc("POST /v1/snapshots/{id}/export", s.requireToken(s.requireUnlocked(s.snapshotExport)))
	mux.HandleFunc("GET /v1/snapshots/{id}", s.requireToken(s.requireUnlocked(s.snapshotGet)))
	mux.HandleFunc("PATCH /v1/snapshots/{id}", s.requireToken(s.requireUnlocked(s.snapshotUpdate)))
	mux.HandleFunc("GET /v1/snapshots/{id}/cookies", s.requireToken(s.requireUnlocked(s.snapshotCookies)))
	mux.HandleFunc("DELETE /v1/snapshots/{id}", s.requireToken(s.requireUnlocked(s.snapshotDelete)))
	mux.HandleFunc("POST /v1/snapshots/{id}/touch", s.requireToken(s.requireUnlocked(s.snapshotTouch)))
	mux.HandleFunc("GET /v1/settings", s.requireToken(s.settingsGet))
	mux.HandleFunc("POST /v1/settings", s.requireToken(s.settingsPost))
	mux.HandleFunc("POST /v1/backup/export", s.requireToken(s.requireUnlocked(s.backupExport)))
	mux.HandleFunc("POST /v1/backup/import", s.requireToken(s.requireUnlocked(s.backupImport)))
	return s.guard(mux)
}

// --- guards ---------------------------------------------------------------

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Host must be loopback or a configured allowed host (reverse proxy).
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		host = strings.ToLower(host)
		if host != "127.0.0.1" && host != "localhost" && host != "::1" && !s.AllowedHosts[host] {
			writeErr(w, http.StatusForbidden, "host_not_allowed")
			return
		}
		// Origin, when present, must be an extension origin or loopback.
		if origin := r.Header.Get("Origin"); origin != "" {
			if !allowedOrigin(origin) {
				writeErr(w, http.StatusForbidden, "origin_not_allowed")
				return
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowedOrigin(origin string) bool {
	if strings.HasPrefix(origin, "chrome-extension://") ||
		strings.HasPrefix(origin, "extension://") ||
		strings.HasPrefix(origin, "moz-extension://") {
		return true
	}
	if strings.HasPrefix(origin, "http://127.0.0.1") ||
		strings.HasPrefix(origin, "http://localhost") {
		return true
	}
	return false
}

func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeErr(w, http.StatusUnauthorized, "missing_token")
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		ok, err := s.Store.TokenExists(store.HashToken(token))
		if err != nil || !ok {
			writeErr(w, http.StatusUnauthorized, "invalid_token")
			return
		}
		next(w, r)
	}
}

func (s *Server) requireUnlocked(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Vault.IsUnlocked() {
			writeErr(w, http.StatusLocked, "vault_locked")
			return
		}
		s.TouchActivity()
		next(w, r)
	}
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// wrappedDEKBlob reconstructs WrappedDEK from meta storage.
func (s *Server) loadWrappedDEK() (*vaultcrypto.WrappedDEK, error) {
	salt, ok, err := s.Store.MetaGet("kdf_salt")
	if !ok || err != nil {
		return nil, fmt.Errorf("no salt: %w", err)
	}
	paramsBlob, _, _ := s.Store.MetaGet("kdf_params")
	params, err := vaultcrypto.ParseParams(paramsBlob)
	if err != nil {
		return nil, err
	}
	nonce, _, _ := s.Store.MetaGet("dek_nonce")
	wrapped, _, _ := s.Store.MetaGet("wrapped_dek")
	return &vaultcrypto.WrappedDEK{Salt: salt, Params: params, Nonce: nonce, WrappedDEK: wrapped}, nil
}

func (s *Server) saveWrappedDEK(w *vaultcrypto.WrappedDEK) error {
	if err := s.Store.MetaSet("kdf_salt", w.Salt); err != nil {
		return err
	}
	if err := s.Store.MetaSet("kdf_params", vaultcrypto.ParamsBlob(w.Params)); err != nil {
		return err
	}
	if err := s.Store.MetaSet("dek_nonce", w.Nonce); err != nil {
		return err
	}
	return s.Store.MetaSet("wrapped_dek", w.WrappedDEK)
}

func (s *Server) issueToken(label string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := s.Store.TokenCreate(newID("tok"), store.HashToken(token), label, time.Now()); err != nil {
		return "", err
	}
	return token, nil
}

// --- health / auth -----------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	init, _ := s.Store.Initialized()
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized": init,
		"unlocked":    s.Vault.IsUnlocked(),
		"version":     version,
	})
}

type passwordReq struct {
	Password string `json:"password"`
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	init, _ := s.Store.Initialized()
	if init {
		writeErr(w, http.StatusConflict, "already_initialized")
		return
	}
	var req passwordReq
	if err := decodeJSON(r, &req); err != nil || len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password_too_short")
		return
	}
	wrapped, dek, err := vaultcrypto.NewWrappedDEK(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "crypto_error")
		return
	}
	if err := s.saveWrappedDEK(wrapped); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	s.Vault.Unlock(dek)
	s.TouchActivity()
	token, err := s.issueToken("setup")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "unlocked": true})
}

func (s *Server) authUnlock(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if time.Now().Before(s.lockedUntil) {
		s.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts")
		return
	}
	s.mu.Unlock()

	var req passwordReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	wrapped, err := s.loadWrappedDEK()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "not_initialized")
		return
	}
	dek, err := vaultcrypto.UnwrapDEK(req.Password, wrapped)
	if err != nil {
		s.mu.Lock()
		s.unlockFails++
		if s.unlockFails >= 5 {
			s.lockedUntil = time.Now().Add(30 * time.Second)
			s.unlockFails = 0
		}
		s.mu.Unlock()
		writeErr(w, http.StatusUnauthorized, "wrong_password")
		return
	}
	s.mu.Lock()
	s.unlockFails = 0
	s.mu.Unlock()
	s.Vault.Unlock(dek)
	s.TouchActivity()
	writeJSON(w, http.StatusOK, map[string]any{"unlocked": true})
}

func (s *Server) authLock(w http.ResponseWriter, r *http.Request) {
	s.Vault.Lock()
	writeJSON(w, http.StatusOK, map[string]any{"unlocked": false})
}

type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordReq
	if err := decodeJSON(r, &req); err != nil || len(req.NewPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "password_too_short")
		return
	}
	wrapped, err := s.loadWrappedDEK()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "not_initialized")
		return
	}
	dek, err := vaultcrypto.UnwrapDEK(req.OldPassword, wrapped)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "wrong_password")
		return
	}
	rewrapped, err := vaultcrypto.Rewrap(dek, req.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "crypto_error")
		return
	}
	if err := s.saveWrappedDEK(rewrapped); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- pairing ------------------------------------------------------------------

func (s *Server) pairVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	s.mu.Lock()
	code, exp := s.PairCode, s.PairExp
	s.mu.Unlock()
	if req.Code == "" || req.Code != code || time.Now().After(exp) {
		writeErr(w, http.StatusUnauthorized, "invalid_pair_code")
		return
	}
	token, err := s.issueToken("pairing")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token})
}

func (s *Server) pairCode(w http.ResponseWriter, r *http.Request) {
	code, err := newPairCode()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "crypto_error")
		return
	}
	s.mu.Lock()
	s.PairCode = code
	s.PairExp = time.Now().Add(5 * time.Minute)
	s.mu.Unlock()
	log.Printf("pair code regenerated: %s (valid 5m)", code)
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_at": s.PairExp.Unix()})
}

func newPairCode() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 8-digit numeric-ish code
	return fmt.Sprintf("%08d", int(b[0])<<24|int(b[1])<<16|int(b[2])<<8|int(b[3])%100000000), nil
}

// --- profiles ------------------------------------------------------------------

func (s *Server) profileList(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ProfileList()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) profileCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name_required")
		return
	}
	now := nowMillis()
	p := store.Profile{ID: newID("p"), Name: req.Name, Note: req.Note, CreatedAt: now, UpdatedAt: now}
	if err := s.Store.ProfileCreate(p); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) profileDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.ProfileDelete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- snapshots ------------------------------------------------------------------

type snapshotCreateReq struct {
	ProfileID string             `json:"profile_id"`
	Domain    string             `json:"domain"`
	Label     string             `json:"label"`
	Note      string             `json:"note"`
	SourceURL string             `json:"source_url"`
	StoreID   string             `json:"store_id"`
	IsReplace *bool              `json:"is_replace"`
	Cookies   []store.CookieInput `json:"cookies"`
}

// mainDomain returns the registrable (main) domain of a host, using a
// small built-in public-suffix heuristic (co.uk/com.cn style suffixes
// take 3 labels, everything else 2). IPs and localhost are returned as-is.
var secondLevelSuffixes = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true,
	"com.cn": true, "net.cn": true, "org.cn": true, "gov.cn": true, "edu.cn": true,
	"com.au": true, "net.au": true, "org.au": true,
	"co.jp": true, "ne.jp": true, "co.kr": true, "com.hk": true, "com.tw": true,
	"co.nz": true, "co.za": true, "com.br": true,
}

func mainDomain(host string) string {
	host = strings.TrimSpace(strings.ToLower(strings.TrimPrefix(host, ".")))
	if host == "" || host == "localhost" || net.ParseIP(host) != nil {
		return host
	}
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	suffix := parts[len(parts)-2] + "." + parts[len(parts)-1]
	if secondLevelSuffixes[suffix] {
		if len(parts) <= 3 {
			return host
		}
		return strings.Join(parts[len(parts)-3:], ".")
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// ensureMainDomainProfile finds or creates the auto-group profile
// named after the site's main domain.
func (s *Server) ensureMainDomainProfile(domain string) string {
	main := mainDomain(domain)
	if main == "" {
		return ""
	}
	if p, ok, _ := s.Store.ProfileByName(main); ok {
		return p.ID
	}
	now := nowMillis()
	p := store.Profile{ID: newID("p"), Name: main, Note: "auto: grouped by main domain", CreatedAt: now, UpdatedAt: now}
	if err := s.Store.ProfileCreate(p); err != nil {
		return ""
	}
	return p.ID
}

func (s *Server) snapshotGroups(w http.ResponseWriter, r *http.Request) {
	raw, err := s.Store.SnapshotDomainCounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	agg := map[string]int{}
	for _, g := range raw {
		agg[mainDomain(g.MainDomain)] += g.Count
	}
	groups := make([]store.GroupCount, 0, len(agg))
	for main, n := range agg {
		groups = append(groups, store.GroupCount{MainDomain: main, Count: n})
	}
	// Sort by count descending, then name.
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].MainDomain < groups[j].MainDomain
	})
	writeJSON(w, http.StatusOK, map[string]any{"items": groups})
}

func (s *Server) snapshotCreate(w http.ResponseWriter, r *http.Request) {
	var req snapshotCreateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if strings.TrimSpace(req.Label) == "" {
		writeErr(w, http.StatusBadRequest, "label_required")
		return
	}
	if req.Domain == "" && len(req.Cookies) > 0 {
		req.Domain = req.Cookies[0].Domain
	}
	if req.Domain == "" {
		writeErr(w, http.StatusBadRequest, "domain_required")
		return
	}

	encCookies := make([]store.EncryptedCookie, 0, len(req.Cookies))
	var fpParts []string
	httpOnly := 0
	// Deduplicate within this snapshot by (name,domain,path,partitioned,store).
	seen := map[string]bool{}
	for _, c := range req.Cookies {
		key := c.Name + "\x00" + c.Domain + "\x00" + c.Path + "\x00" + c.PartitionKey
		if seen[key] {
			continue
		}
		seen[key] = true
		nonce, enc, err := s.Vault.Encrypt(c.Value)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "crypto_error")
			return
		}
		var exp *int64
		if c.ExpirationDate != nil {
			v := int64(*c.ExpirationDate)
			exp = &v
		}
		ec := store.EncryptedCookie{
			Input: c, ValueEnc: enc, Nonce: nonce,
			Hash: store.Fingerprint([]string{c.Name, c.Domain, c.Path, c.Value}),
			ExpiresAt: exp,
		}
		encCookies = append(encCookies, ec)
		fpParts = append(fpParts, c.Name, c.Domain, c.Path, c.Value)
		if c.HTTPOnly {
			httpOnly++
		}
	}

	isReplace := true
	if req.IsReplace != nil {
		isReplace = *req.IsReplace
	}
	// Auto-group by the site's main domain when no profile was given.
	profileID := req.ProfileID
	if profileID == "" {
		profileID = s.ensureMainDomainProfile(req.Domain)
	}
	now := nowMillis()
	snap := store.Snapshot{
		ID: newID("s"), ProfileID: profileID, Domain: req.Domain,
		Label: req.Label, Note: req.Note, SourceURL: req.SourceURL,
		StoreID: req.StoreID, IsReplace: isReplace,
		Fingerprint: store.Fingerprint(fpParts),
		CookieCount: len(encCookies), HTTPOnlyCount: httpOnly,
		CreatedAt: now, LastUsedAt: now,
	}
	if err := s.Store.SnapshotCreate(snap, encCookies); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	s.Store.Audit("snapshot_create", snap.ID, snap.Domain+" "+snap.Label, time.Now())
	writeJSON(w, http.StatusCreated, snap)
}

func (s *Server) snapshotList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	// cursor is base64url("<key>\n<id>") produced by encodeCursorObj.
	var cursorKey, cursorID string
	if cur := q.Get("cursor"); cur != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cur)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_cursor")
			return
		}
		k, id, ok := store.DecodeCursor(string(raw))
		if !ok {
			writeErr(w, http.StatusBadRequest, "bad_cursor")
			return
		}
		cursorKey, cursorID = k, id
	}
	lq := store.ListQuery{
		Q: q.Get("q"), Domain: q.Get("domain"), ProfileID: q.Get("profile_id"),
		Sort: q.Get("sort"), Limit: limit, CursorKey: cursorKey, CursorID: cursorID,
	}
	total, err := s.Store.SnapshotCount(lq)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	res, err := s.Store.SnapshotList(lq)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	out := map[string]any{
		"items": res.Items, "has_more": res.HasMore, "total": total,
	}
	if res.HasMore {
		out["next_cursor"] = base64.RawURLEncoding.EncodeToString([]byte(res.NextCursor))
	} else {
		out["next_cursor"] = ""
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) snapshotGet(w http.ResponseWriter, r *http.Request) {
	snap, ok, err := s.Store.SnapshotGet(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) snapshotUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label     string `json:"label"`
		Note      string `json:"note"`
		IsReplace *bool  `json:"is_replace"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	id := r.PathValue("id")
	if _, ok, _ := s.Store.SnapshotGet(id); !ok {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	// Allow partial update: empty label keeps existing.
	snap, _, _ := s.Store.SnapshotGet(id)
	label := req.Label
	if label == "" {
		label = snap.Label
	}
	if err := s.Store.SnapshotUpdate(id, label, req.Note); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	if req.IsReplace != nil {
		_ = s.Store.SnapshotSetReplace(id, *req.IsReplace)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type cookieOut struct {
	Name           string  `json:"name"`
	Value          string  `json:"value"`
	Domain         string  `json:"domain"`
	HostOnly       bool    `json:"hostOnly"`
	Path           string  `json:"path"`
	Secure         bool    `json:"secure"`
	HTTPOnly       bool    `json:"httpOnly"`
	SameSite       string  `json:"sameSite"`
	ExpirationDate *float64 `json:"expirationDate,omitempty"`
	PartitionKey   string  `json:"partitionKey,omitempty"`
	Partitioned    bool    `json:"partitioned,omitempty"`
}

func (s *Server) snapshotCookies(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok, _ := s.Store.SnapshotGet(id); !ok {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	stored, err := s.Store.SnapshotCookies(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	filter := strings.ToLower(r.URL.Query().Get("q"))
	items := make([]cookieOut, 0, len(stored))
	for _, c := range stored {
		val, err := s.Vault.Decrypt(c.Nonce, c.ValueEnc)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "decrypt_error")
			return
		}
		if filter != "" && !strings.Contains(strings.ToLower(c.Name), filter) &&
			!strings.Contains(strings.ToLower(c.Domain), filter) {
			continue
		}
		var exp *float64
		if c.ExpiresAt != nil {
			v := float64(*c.ExpiresAt)
			exp = &v
		}
		items = append(items, cookieOut{
			Name: c.Name, Value: val, Domain: c.Domain, HostOnly: c.HostOnly,
			Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly,
			SameSite: c.SameSite, ExpirationDate: exp,
			PartitionKey: c.PartitionKey, Partitioned: c.Partitioned,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func (s *Server) snapshotDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.SnapshotDelete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	s.Store.Audit("snapshot_delete", id, "", time.Now())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) snapshotTouch(w http.ResponseWriter, r *http.Request) {
	s.Store.Audit("snapshot_restore_touch", r.PathValue("id"), "", time.Now())
	if err := s.Store.SnapshotTouch(r.PathValue("id"), time.Now()); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- settings (idle auto-lock) ----------------------------------------------

const defaultIdleLockMinutes = 15

func (s *Server) idleLockMinutes() int {
	v, ok, err := s.Store.MetaGet("idle_lock_minutes")
	if err != nil || !ok {
		return defaultIdleLockMinutes
	}
	n, err := strconv.Atoi(string(v))
	if err != nil || n < 0 {
		return defaultIdleLockMinutes
	}
	return n
}

func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"idle_lock_minutes": s.idleLockMinutes()})
}

func (s *Server) settingsPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IdleLockMinutes *int `json:"idle_lock_minutes"`
	}
	if err := decodeJSON(r, &req); err != nil || req.IdleLockMinutes == nil || *req.IdleLockMinutes < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if err := s.Store.MetaSet("idle_lock_minutes", []byte(strconv.Itoa(*req.IdleLockMinutes))); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"idle_lock_minutes": *req.IdleLockMinutes})
}

// --- backup export / import ----------------------------------------------------
//
// A backup file is a JSON container whose payload is encrypted directly
// under a backup password (Argon2id + AES-GCM), independent of the vault
// DEK, so it can be imported on another installation. The plaintext payload
// contains profiles + snapshots + decrypted cookie values.

type backupSnapshot struct {
	Snapshot store.Snapshot        `json:"snapshot"`
	Cookies  []store.CookieInput  `json:"cookies"`
}
type backupPlain struct {
	Version    int                `json:"version"`
	ExportedAt int64              `json:"exported_at"`
	Profiles   []store.Profile    `json:"profiles"`
	Snapshots  []backupSnapshot   `json:"snapshots"`
}
type backupContainer struct {
	Version    int                        `json:"version"`
	KDF        vaultcrypto.PasswordEncrypted `json:"kdf"`
}

// buildPlain assembles the decryptable backup payload for the given
// snapshots, including their profiles.
func (s *Server) buildPlain(snaps []store.Snapshot) (backupPlain, error) {
	profiles, err := s.Store.ProfileList()
	if err != nil {
		return backupPlain{}, err
	}
	plain := backupPlain{Version: 1, ExportedAt: time.Now().Unix(), Profiles: profiles, Snapshots: []backupSnapshot{}}
	for _, snap := range snaps {
		stored, err := s.Store.SnapshotCookies(snap.ID)
		if err != nil {
			return backupPlain{}, err
		}
		bs := backupSnapshot{Snapshot: snap, Cookies: []store.CookieInput{}}
		for _, c := range stored {
			val, err := s.Vault.Decrypt(c.Nonce, c.ValueEnc)
			if err != nil {
				return backupPlain{}, err
			}
			ci := store.CookieInput{
				Name: c.Name, Value: val, Domain: c.Domain, HostOnly: c.HostOnly,
				Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly,
				SameSite: c.SameSite, PartitionKey: c.PartitionKey, Partitioned: c.Partitioned,
			}
			if c.ExpiresAt != nil {
				v := float64(*c.ExpiresAt)
				ci.ExpirationDate = &v
			}
			bs.Cookies = append(bs.Cookies, ci)
		}
		plain.Snapshots = append(plain.Snapshots, bs)
	}
	return plain, nil
}

func (s *Server) writeEncryptedBackup(w http.ResponseWriter, plain backupPlain, password, auditSnapID, auditAction string) {
	raw, err := json.Marshal(plain)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode_error")
		return
	}
	enc, err := vaultcrypto.EncryptWithPassword(password, raw)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "crypto_error")
		return
	}
	s.Store.Audit(auditAction, auditSnapID, fmt.Sprintf("%d snapshots", len(plain.Snapshots)), time.Now())
	writeJSON(w, http.StatusOK, backupContainer{Version: 1, KDF: *enc})
}

func (s *Server) backupExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BackupPassword string `json:"backup_password"`
	}
	if err := decodeJSON(r, &req); err != nil || len(req.BackupPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "backup_password_too_short")
		return
	}
	snaps, err := s.Store.SnapshotAll()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	plain, err := s.buildPlain(snaps)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	s.writeEncryptedBackup(w, plain, req.BackupPassword, "", "backup_export")
}

// snapshotExport exports a single snapshot as an encrypted backup package
// (same format as backupExport, so it can be imported with /backup/import).
func (s *Server) snapshotExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BackupPassword string `json:"backup_password"`
	}
	if err := decodeJSON(r, &req); err != nil || len(req.BackupPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "backup_password_too_short")
		return
	}
	snap, ok, err := s.Store.SnapshotGet(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	plain, err := s.buildPlain([]store.Snapshot{snap})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error")
		return
	}
	s.writeEncryptedBackup(w, plain, req.BackupPassword, snap.ID, "snapshot_export")
}

// --- snapshot compare ---------------------------------------------------------
//
// Compares two snapshots cookie-by-cookie. Returns only cookie identity
// and which fields changed — never cookie values.

type diffCookie struct {
	Name   string   `json:"name"`
	Domain string   `json:"domain"`
	Path   string   `json:"path"`
	Fields []string `json:"fields,omitempty"` // for changed entries
}

func cookieIdentity(c store.CookieInput) string {
	return c.Name + "\x00" + c.Domain + "\x00" + c.Path + "\x00" + c.PartitionKey
}

func (s *Server) decryptedCookies(id string) (map[string]store.CookieInput, error) {
	stored, err := s.Store.SnapshotCookies(id)
	if err != nil {
		return nil, err
	}
	out := map[string]store.CookieInput{}
	for _, c := range stored {
		val, err := s.Vault.Decrypt(c.Nonce, c.ValueEnc)
		if err != nil {
			return nil, err
		}
		ci := store.CookieInput{
			Name: c.Name, Value: val, Domain: c.Domain, HostOnly: c.HostOnly,
			Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly,
			SameSite: c.SameSite, PartitionKey: c.PartitionKey, Partitioned: c.Partitioned,
		}
		if c.ExpiresAt != nil {
			v := float64(*c.ExpiresAt)
			ci.ExpirationDate = &v
		}
		out[cookieIdentity(ci)] = ci
	}
	return out, nil
}

func (s *Server) snapshotCompare(w http.ResponseWriter, r *http.Request) {
	aID, bID := r.URL.Query().Get("a"), r.URL.Query().Get("b")
	if aID == "" || bID == "" || aID == bID {
		writeErr(w, http.StatusBadRequest, "a_and_b_required")
		return
	}
	aSnap, okA, _ := s.Store.SnapshotGet(aID)
	bSnap, okB, _ := s.Store.SnapshotGet(bID)
	if !okA || !okB {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	aMap, err := s.decryptedCookies(aID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "decrypt_error")
		return
	}
	bMap, err := s.decryptedCookies(bID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "decrypt_error")
		return
	}
	added := []diffCookie{}
	removed := []diffCookie{}
	changed := []diffCookie{}
	unchanged := 0
	for key, b := range bMap {
		a, inA := aMap[key]
		if !inA {
			added = append(added, diffCookie{Name: b.Name, Domain: b.Domain, Path: b.Path})
			continue
		}
		var fields []string
		if a.Value != b.Value {
			fields = append(fields, "value")
		}
		if a.Secure != b.Secure {
			fields = append(fields, "secure")
		}
		if a.HTTPOnly != b.HTTPOnly {
			fields = append(fields, "httpOnly")
		}
		if !strings.EqualFold(a.SameSite, b.SameSite) {
			fields = append(fields, "sameSite")
		}
		if a.HostOnly != b.HostOnly {
			fields = append(fields, "hostOnly")
		}
		if len(fields) > 0 {
			changed = append(changed, diffCookie{Name: b.Name, Domain: b.Domain, Path: b.Path, Fields: fields})
		} else {
			unchanged++
		}
	}
	for key, a := range aMap {
		if _, inB := bMap[key]; !inB {
			removed = append(removed, diffCookie{Name: a.Name, Domain: a.Domain, Path: a.Path})
		}
	}
	s.Store.Audit("snapshot_compare", aID, "vs "+bID, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"a": map[string]any{"id": aSnap.ID, "label": aSnap.Label, "domain": aSnap.Domain, "cookie_count": aSnap.CookieCount},
		"b": map[string]any{"id": bSnap.ID, "label": bSnap.Label, "domain": bSnap.Domain, "cookie_count": bSnap.CookieCount},
		"added": added, "removed": removed, "changed": changed, "unchanged": unchanged,
	})
}

func (s *Server) backupImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BackupPassword string          `json:"backup_password"`
		SkipDuplicates *bool           `json:"skip_duplicates"`
		Data           backupContainer `json:"data"`
	}
	if err := decodeJSON(r, &req); err != nil || len(req.BackupPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	raw, err := vaultcrypto.DecryptWithPassword(req.BackupPassword, &req.Data.KDF)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "wrong_backup_password")
		return
	}
	var plain backupPlain
	if err := json.Unmarshal(raw, &plain); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_backup")
		return
	}
	skipDup := true
	if req.SkipDuplicates != nil {
		skipDup = *req.SkipDuplicates
	}
	// Remap profiles by name (create missing ones).
	profileMap := map[string]string{}
	now := nowMillis()
	for _, p := range plain.Profiles {
		if existing, ok, _ := s.Store.ProfileByName(p.Name); ok {
			profileMap[p.ID] = existing.ID
			continue
		}
		newP := store.Profile{ID: newID("p"), Name: p.Name, Note: p.Note, CreatedAt: now, UpdatedAt: now}
		if err := s.Store.ProfileCreate(newP); err != nil {
			writeErr(w, http.StatusInternalServerError, "storage_error")
			return
		}
		profileMap[p.ID] = newP.ID
	}
	importedSnaps, importedCookies, skipped := 0, 0, 0
	for _, bs := range plain.Snapshots {
		old := bs.Snapshot
		if skipDup {
			if exists, _ := s.Store.SnapshotExistsFingerprint(old.Domain, old.Label, old.Fingerprint); exists {
				skipped++
				continue
			}
		}
		encCookies := make([]store.EncryptedCookie, 0, len(bs.Cookies))
		var fpParts []string
		httpOnly := 0
		for _, c := range bs.Cookies {
			nonce, enc, err := s.Vault.Encrypt(c.Value)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "crypto_error")
				return
			}
			var exp *int64
			if c.ExpirationDate != nil {
				v := int64(*c.ExpirationDate)
				exp = &v
			}
			encCookies = append(encCookies, store.EncryptedCookie{
				Input: c, ValueEnc: enc, Nonce: nonce,
				Hash: store.Fingerprint([]string{c.Name, c.Domain, c.Path, c.Value}), ExpiresAt: exp,
			})
			fpParts = append(fpParts, c.Name, c.Domain, c.Path, c.Value)
			if c.HTTPOnly {
				httpOnly++
			}
		}
		fingerprint := old.Fingerprint
		if fingerprint == "" && len(fpParts) > 0 {
			fingerprint = store.Fingerprint(fpParts)
		}
		snap := store.Snapshot{
			ID: newID("s"), Domain: old.Domain, Label: old.Label, Note: old.Note,
			SourceURL: old.SourceURL, StoreID: old.StoreID, IsReplace: old.IsReplace,
			Fingerprint: fingerprint, CookieCount: len(encCookies), HTTPOnlyCount: httpOnly,
			CreatedAt: now, LastUsedAt: now,
		}
		if pid, ok := profileMap[old.ProfileID]; ok {
			snap.ProfileID = pid
		}
		if err := s.Store.SnapshotCreate(snap, encCookies); err != nil {
			writeErr(w, http.StatusInternalServerError, "storage_error")
			return
		}
		importedSnaps++
		importedCookies += len(encCookies)
	}
	s.Store.Audit("backup_import", "", fmt.Sprintf("%d snapshots", importedSnaps), time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"imported_snapshots": importedSnaps, "imported_cookies": importedCookies, "skipped": skipped,
	})
}
