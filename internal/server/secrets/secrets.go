// Package secrets encrypts values at rest with AES-256-GCM. Ciphertext carries a key id so
// keys can be rotated: decryption works with any known key, encryption uses the current one.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNoKey means the keyring has no key for the ciphertext's key id.
var ErrNoKey = errors.New("secrets: unknown key id")

// Keyring holds the current key and any older keys still needed for decryption.
type Keyring struct {
	current string
	keys    map[string][]byte
}

// LoadKeyring reads GATOR_SECRETS_KEY (current, "id:hex") and GATOR_SECRETS_OLD_KEYS
// (comma-separated "id:hex" entries). Empty current key disables encryption at rest.
func LoadKeyring() (*Keyring, error) {
	return ParseKeyring(os.Getenv("GATOR_SECRETS_KEY"), os.Getenv("GATOR_SECRETS_OLD_KEYS"))
}

// ParseKeyring builds a keyring from "id:hex" strings.
func ParseKeyring(current, old string) (*Keyring, error) {
	if current == "" {
		return nil, errors.New("GATOR_SECRETS_KEY is required (generate with `gator-server secrets new-key`)")
	}
	k := &Keyring{keys: map[string][]byte{}}
	id, key, err := parseKey(current)
	if err != nil {
		return nil, err
	}
	k.current = id
	k.keys[id] = key
	for _, entry := range strings.Split(old, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, key, err := parseKey(entry)
		if err != nil {
			return nil, err
		}
		k.keys[id] = key
	}
	return k, nil
}

func parseKey(s string) (string, []byte, error) {
	id, hexKey, ok := strings.Cut(s, ":")
	if !ok || id == "" {
		return "", nil, fmt.Errorf("secrets: key must be \"id:hex\", got %q", s)
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil || len(key) != 32 {
		return "", nil, fmt.Errorf("secrets: key %q must be 32 bytes hex", id)
	}
	return id, key, nil
}

// NewKey generates a random 32-byte key formatted for the environment.
func NewKey(id string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return id + ":" + hex.EncodeToString(b)
}

// CurrentID returns the id of the encryption key.
func (k *Keyring) CurrentID() string { return k.current }

// Encrypt returns "v1.<keyid>.<base64 nonce+ciphertext>".
func (k *Keyring) Encrypt(plaintext []byte) (string, error) {
	gcm, err := k.gcm(k.current)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, []byte(k.current))
	return "v1." + k.current + "." + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt with whichever key the ciphertext names.
func (k *Keyring) Decrypt(ciphertext string) ([]byte, error) {
	parts := strings.SplitN(ciphertext, ".", 3)
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, errors.New("secrets: malformed ciphertext")
	}
	keyID := parts[1]
	gcm, err := k.gcm(keyID)
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("secrets: ciphertext too short")
	}
	nonce, sealed := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	return gcm.Open(nil, nonce, sealed, []byte(keyID))
}

// KeyID reports which key a ciphertext uses.
func KeyID(ciphertext string) string {
	parts := strings.SplitN(ciphertext, ".", 3)
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

// NeedsRotation reports whether ciphertext was sealed with an older key.
func (k *Keyring) NeedsRotation(ciphertext string) bool { return KeyID(ciphertext) != k.current }

// Rotate re-encrypts ciphertext under the current key.
func (k *Keyring) Rotate(ciphertext string) (string, error) {
	plain, err := k.Decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return k.Encrypt(plain)
}

func (k *Keyring) gcm(id string) (cipher.AEAD, error) {
	key, ok := k.keys[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoKey, id)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
