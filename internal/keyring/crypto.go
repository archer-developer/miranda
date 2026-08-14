package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/argon2"
)

const (
	masterKeySize  = 32
	argon2SaltSize = 16
	argon2KeySize  = 32

	// Argon2id parameters for the password-fallback KDF. Kept as constants
	// rather than a config knob — this is security-sensitive and easy to
	// misconfigure into uselessness, with no real per-deployment reason to
	// differ. Persisted per-row as a versioned string (currentKDFParams)
	// rather than read fresh at unwrap time, so a future tuning change can't
	// silently break existing rows.
	argon2Time        = 1
	argon2MemoryKiB   = 64 * 1024 // 64 MiB
	argon2Parallelism = 4
)

// kdfParams is the Argon2id tuning used to derive a wrapping key from a
// user's plaintext password. Every password slot stores its own params
// string (see currentKDFParams/parseKDFParams) so historical rows keep
// working even if defaultKDFParams changes later.
type kdfParams struct {
	memoryKiB   uint32
	time        uint32
	parallelism uint8
}

func defaultKDFParams() kdfParams {
	return kdfParams{memoryKiB: argon2MemoryKiB, time: argon2Time, parallelism: argon2Parallelism}
}

// currentKDFParams renders defaultKDFParams as the versioned string stored
// in a password slot's kdf_params column — the same spirit as bcrypt
// embedding its own cost in the hash string.
func currentKDFParams() string {
	p := defaultKDFParams()
	return fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d", argon2.Version, p.memoryKiB, p.time, p.parallelism)
}

// parseKDFParams reverses currentKDFParams, so a password slot is always
// re-derived using whatever params it was created with, not today's
// defaults.
func parseKDFParams(s string) (kdfParams, error) {
	var version int
	var p kdfParams
	var pp uint32
	if _, err := fmt.Sscanf(s, "argon2id$v=%d$m=%d,t=%d,p=%d", &version, &p.memoryKiB, &p.time, &pp); err != nil {
		return kdfParams{}, fmt.Errorf("keyring: parse kdf params %q: %w", s, err)
	}
	p.parallelism = uint8(pp)
	return p, nil
}

// GenerateMasterKey returns a fresh random 32-byte (AES-256) master key K.
func GenerateMasterKey() ([]byte, error) {
	key := make([]byte, masterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("keyring: generate master key: %w", err)
	}
	return key, nil
}

// newSalt returns a fresh random salt for a password slot's Argon2id
// derivation — independent of, and never reused from, the bcrypt salt
// embedded in users.User.PasswordHash.
func newSalt() ([]byte, error) {
	salt := make([]byte, argon2SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("keyring: generate salt: %w", err)
	}
	return salt, nil
}

// deriveFromPassword runs Argon2id over the plaintext password with the
// given salt/params, producing a 32-byte AES-256 wrapping key. Never
// persisted — only ever held transiently in memory during a login request.
func deriveFromPassword(password string, salt []byte, params kdfParams) []byte {
	return argon2.IDKey([]byte(password), salt, params.time, params.memoryKiB, params.parallelism, argon2KeySize)
}

// slotAAD binds a wrapped key to its own row identity (username, slot type,
// slot ref) as AES-GCM associated data. Without this, an attacker with disk
// access could splice one row's wrapped_key+nonce into a different row and,
// given a correctly-derived key for the *target* row, have it decrypt there
// undetected; with AAD bound to the row's own identity, gcm.Open fails
// loudly (authentication tag mismatch) on any such swap.
func slotAAD(username, slotType, slotRef string) []byte {
	return []byte(username + "|" + slotType + "|" + slotRef)
}

// wrap encrypts plaintext (typically a 32-byte master key) under key using
// AES-256-GCM with a fresh random nonce, returning the ciphertext (with
// GCM's authentication tag appended) and the nonce used.
func wrap(key, plaintext, aad []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("keyring: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("keyring: new gcm: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("keyring: generate nonce: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, aad)
	return ciphertext, nonce, nil
}

// unwrap reverses wrap. Fails (authentication tag mismatch) if key, aad,
// ciphertext, or nonce don't all match what wrap produced together — this
// is what makes a wrong password/PRF output, or a tampered/spliced row,
// fail loudly instead of returning garbage.
func unwrap(key, ciphertext, nonce, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("keyring: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keyring: new gcm: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("keyring: unwrap: %w", err)
	}
	return plaintext, nil
}

// Wrap and Unwrap are the exported form of wrap/unwrap above — the same
// AES-256-GCM primitives, usable by any package that needs the identical
// encrypt-at-rest shape without duplicating crypto code (e.g. internal/oauth2's
// refresh-token encryption — see docs/adr/oauth2-layer.md). Kept as thin
// exported wrappers rather than renaming wrap/unwrap themselves, so every
// existing call site inside this package is untouched.
func Wrap(key, plaintext, aad []byte) (ciphertext, nonce []byte, err error) {
	return wrap(key, plaintext, aad)
}

func Unwrap(key, ciphertext, nonce, aad []byte) ([]byte, error) {
	return unwrap(key, ciphertext, nonce, aad)
}
