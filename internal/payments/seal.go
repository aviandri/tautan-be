package payments

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrNoKey means PAYMENTS_ENCRYPTION_KEY is not set, so provider credentials
// can be neither stored nor read.
var ErrNoKey = errors.New("payments: PAYMENTS_ENCRYPTION_KEY is not configured")

// sealVersion is the first byte of every ciphertext.
const sealVersion = 1

// Sealer encrypts provider credentials at rest with AES-256-GCM.
//
// Ciphertext layout: version (1 byte) | key id (4 bytes) | nonce (12) | sealed.
// The key id is the first 4 bytes of sha256(key), so after a rotation old rows
// still open with a retired key listed in PAYMENTS_ENCRYPTION_KEYS_OLD, and the
// next save re-seals them with the current key.
type Sealer struct {
	current keyEntry
	old     []keyEntry
}

type keyEntry struct {
	id   [4]byte
	aead cipher.AEAD
}

// NewSealer parses the current key and any retired keys. Each key is 32 bytes,
// given as 64 hex characters or standard/URL base64 (padded or not).
// An empty current key returns (nil, nil): payments are then disabled.
func NewSealer(current string, old []string) (*Sealer, error) {
	if strings.TrimSpace(current) == "" {
		return nil, nil
	}
	cur, err := parseKey(current)
	if err != nil {
		return nil, fmt.Errorf("PAYMENTS_ENCRYPTION_KEY: %w", err)
	}
	s := &Sealer{current: cur}
	for i, k := range old {
		if strings.TrimSpace(k) == "" {
			continue
		}
		e, err := parseKey(k)
		if err != nil {
			return nil, fmt.Errorf("PAYMENTS_ENCRYPTION_KEYS_OLD[%d]: %w", i, err)
		}
		s.old = append(s.old, e)
	}
	return s, nil
}

func parseKey(s string) (keyEntry, error) {
	s = strings.TrimSpace(s)
	var raw []byte
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		raw = b
	} else {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
				raw = b
				break
			}
		}
	}
	if raw == nil {
		return keyEntry{}, errors.New("must be 32 bytes, as 64 hex characters or base64 (try: openssl rand -base64 32)")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return keyEntry{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return keyEntry{}, err
	}
	sum := sha256.Sum256(raw)
	var e keyEntry
	copy(e.id[:], sum[:4])
	e.aead = aead
	return e, nil
}

// Seal encrypts plaintext. aad binds the ciphertext to its row (profile,
// provider, mode) so a ciphertext copied to another row fails to open.
func (s *Sealer) Seal(plaintext, aad []byte) ([]byte, error) {
	if s == nil {
		return nil, ErrNoKey
	}
	nonce := make([]byte, s.current.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 5+len(nonce)+len(plaintext)+s.current.aead.Overhead())
	out = append(out, sealVersion)
	out = append(out, s.current.id[:]...)
	out = append(out, nonce...)
	return s.current.aead.Seal(out, nonce, plaintext, aad), nil
}

// Open decrypts a ciphertext made by Seal with the current or a retired key.
// stale reports that a retired key was used, so the caller should re-seal.
func (s *Sealer) Open(ciphertext, aad []byte) (plaintext []byte, stale bool, err error) {
	if s == nil {
		return nil, false, ErrNoKey
	}
	if len(ciphertext) < 5 || ciphertext[0] != sealVersion {
		return nil, false, errors.New("payments: unknown ciphertext format")
	}
	var id [4]byte
	copy(id[:], ciphertext[1:5])
	body := ciphertext[5:]
	for i, k := range append([]keyEntry{s.current}, s.old...) {
		if k.id != id {
			continue
		}
		ns := k.aead.NonceSize()
		if len(body) < ns {
			return nil, false, errors.New("payments: ciphertext too short")
		}
		pt, err := k.aead.Open(nil, body[:ns], body[ns:], aad)
		if err != nil {
			return nil, false, errors.New("payments: ciphertext failed authentication")
		}
		return pt, i > 0, nil
	}
	return nil, false, errors.New("payments: ciphertext was sealed with an unknown key")
}
