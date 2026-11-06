// CookieVault server: localhost-only Go service that stores browser
// cookies (encrypted with a user master password) for the MV3 extension.
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"cookievault/server/internal/api"
	vaultcrypto "cookievault/server/internal/crypto"
	"cookievault/server/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18765", "listen address (loopback only)")
	dataDir := flag.String("data-dir", envOr("COOKIEVAULT_DATA_DIR", "data"), "directory for SQLite DB")
	allowedHosts := flag.String("allowed-hosts", envOr("COOKIEVAULT_ALLOWED_HOSTS", ""),
		"comma-separated extra Host values to accept (reverse proxy / public domain), e.g. edge02.10w123.com")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	dbPath := filepath.Join(*dataDir, "cookievault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	pairCode := genPairCode()
	v := &vaultcrypto.Vault{}
	srv := api.New(st, v, pairCode)
	srv.SetAllowedHosts(*allowedHosts)

	init, _ := st.Initialized()
	log.Printf("cookievault listening on %s (db: %s, initialized=%v)", *addr, dbPath, init)
	log.Printf("pair code: %s (valid 5 minutes, enter it in the extension options)", pairCode)
	if !init {
		log.Printf("first run: set a master password via POST /v1/auth/setup (min 8 chars)")
	}

	// Idle auto-lock: check every 30s; lock when no authenticated
	// activity for idle_lock_minutes (0 = disabled, default 15).
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if !v.IsUnlocked() {
				continue
			}
			v2, ok, _ := st.MetaGet("idle_lock_minutes")
			minutes := 15
			if ok {
				var n int
				if _, err := fmt.Sscanf(string(v2), "%d", &n); err == nil {
					minutes = n
				}
			}
			if minutes <= 0 {
				continue
			}
			last := srv.LastActivity()
			if last == 0 {
				continue
			}
			if time.Now().Unix()-last > int64(minutes)*60 {
				v.Lock()
				log.Printf("idle auto-lock after %d min", minutes)
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func genPairCode() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return fmt.Sprintf("%08d", int(b[0])<<24|int(b[1])<<16|int(b[2])<<8|int(b[3])%100000000)
}
