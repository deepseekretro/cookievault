// Package crypto implements envelope encryption for CookieVault.
//
// Layout:
//   - A random DEK (data encryption key) encrypts every cookie value with
//     AES-256-GCM (random nonce per value).
//   - The DEK is "wrapped" (encrypted) with a KEK derived from the user's
//     master password via Argon2id.
//   - Changing the master password only re-wraps the DEK; cookie values
//     are never re-encrypted.
//   - The DEK lives in memory only while unlocked, and is zeroed on lock.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"sync"

	"golang.org/x/crypto/argon2"
)

var ErrLocked = errors.New("vault is locked")
var ErrWrongPassword = errors.New("wrong master password")

const (
	keyLen   = 32 // AES-256
	saltLen  = 16
	nonceLen = 12 // GCM standard nonce
)

// Argon2id parameters. Memory is in KiB.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
)

// Params are persisted so they can be tuned later without breaking old DBs.
type Params struct {
	Time    uint32
	Memory  uint32
	Threads uint8
}

// DefaultParams returns the current KDF parameters.
func DefaultParams() Params { return Params{Time: argonTime, Memory: argonMemory, Threads: argonThreads} }

// WrappedDEK is the persisted form of the DEK.
type WrappedDEK struct {
	Salt      []byte
	Params    Params
	Nonce     []byte
	WrappedDEK []byte
}

func deriveKEK(password string, salt []byte, p Params) []byte {
	return argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, keyLen)
}

func seal(key, plaintext []byte) (nonce, ciphertext []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, nil), nil
}

func open(key, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// NewWrappedDEK creates a fresh DEK wrapped by password.
func NewWrappedDEK(password string) (*WrappedDEK, []byte, error) {
	dek := make([]byte, keyLen)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	p := DefaultParams()
	kek := deriveKEK(password, salt, p)
	nonce, wrapped, err := seal(kek, dek)
	if err != nil {
		return nil, nil, err
	}
	return &WrappedDEK{Salt: salt, Params: p, Nonce: nonce, WrappedDEK: wrapped}, dek, nil
}

// UnwrapDEK recovers the DEK with password.
func UnwrapDEK(password string, w *WrappedDEK) ([]byte, error) {
	kek := deriveKEK(password, w.Salt, w.Params)
	dek, err := open(kek, w.Nonce, w.WrappedDEK)
	if err != nil {
		return nil, ErrWrongPassword
	}
	if len(dek) != keyLen {
		return nil, ErrWrongPassword
	}
	return dek, nil
}

// Rewrap re-encrypts an existing DEK under a new password.
func Rewrap(dek []byte, newPassword string) (*WrappedDEK, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	p := DefaultParams()
	kek := deriveKEK(newPassword, salt, p)
	nonce, wrapped, err := seal(kek, dek)
	if err != nil {
		return nil, err
	}
	return &WrappedDEK{Salt: salt, Params: p, Nonce: nonce, WrappedDEK: wrapped}, nil
}

// Vault holds the in-memory DEK while unlocked.
type Vault struct {
	mu       sync.RWMutex
	dek      []byte
	unlocked bool
}

// Unlock stores the DEK in memory.
func (v *Vault) Unlock(dek []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lockLocked()
	v.dek = append([]byte(nil), dek...)
	v.unlocked = true
}

// Lock zeroes and drops the DEK.
func (v *Vault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lockLocked()
}

func (v *Vault) lockLocked() {
	if v.dek != nil {
		for i := range v.dek {
			v.dek[i] = 0
		}
		v.dek = nil
	}
	v.unlocked = false
}

// IsUnlocked reports whether the vault is unlocked.
func (v *Vault) IsUnlocked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.unlocked
}

// Encrypt seals plaintext under the DEK.
func (v *Vault) Encrypt(plaintext string) (nonce, ciphertext []byte, err error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.unlocked {
		return nil, nil, ErrLocked
	}
	return seal(v.dek, []byte(plaintext))
}

// Decrypt opens ciphertext sealed under the DEK.
func (v *Vault) Decrypt(nonce, ciphertext []byte) (string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.unlocked {
		return "", ErrLocked
	}
	out, err := open(v.dek, nonce, ciphertext)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ParamsBlob encodes KDF params for storage in meta.
func ParamsBlob(p Params) []byte {
	b := make([]byte, 9)
	binary.BigEndian.PutUint32(b[0:4], p.Time)
	binary.BigEndian.PutUint32(b[4:8], p.Memory)
	b[8] = p.Threads
	return b
}

// ParseParams decodes KDF params.
func ParseParams(b []byte) (Params, error) {
	if len(b) != 9 {
		return Params{}, errors.New("invalid params blob")
	}
	return Params{
		Time:    binary.BigEndian.Uint32(b[0:4]),
		Memory:  binary.BigEndian.Uint32(b[4:8]),
		Threads: b[8],
	}, nil
}

// ConstantTimeEqual is a small helper for comparing secrets.
func ConstantTimeEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }
