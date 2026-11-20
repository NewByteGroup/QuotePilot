package auth

// argon2id password hashing for the ADR-005 amendment 2026-11-16 login path.
// The at-rest format is the PHC string ($argon2id$v=19$m=…,t=…,p=…$salt$key)
// and only that format: parameters ride inside the hash so a future bump
// never invalidates existing rows, and the column stays self-describing.
// Raw passwords never leave this file — the handler sees only verifyPassword's
// bool, and the store only ever carries the PHC string.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"

	"golang.org/x/crypto/argon2"
)

// RFC 9106's memory-constrained argon2id profile (m=19 MiB, t=2, p=1) — also
// OWASP's second recommended parameter set. The first-choice 64 MiB profile
// is for dedicated auth hardware; the pilot API runs in small containers, so
// the bounded variant keeps a concurrent-login burst from becoming a memory
// DoS while still costing an attacker ~19 MiB and two passes per guess.
// Salt and key lengths are the RFC defaults. These constants seed NEW hashes
// only — verifyPassword reads m/t/p from the PHC string itself.
const (
	argonMemoryKiB   = 19 * 1024
	argonTime        = 2
	argonParallelism = 1
	argonSaltLen     = 16
	argonKeyLen      = 32
)

// dummyPasswordHash is a well-formed argon2id PHC string whose key matches no
// password (salt and key are zero bytes — argon2id never derives a zero key).
// passwordLogin verifies against it when the email is unknown or the stored
// hash is NULL/empty: without it those paths would skip the ~50ms argon2id
// run, and a response-time oracle could tell "no such user" or "no password
// set" apart from "wrong password" even though all three share one 401 body
// (ADR-005 amendment — same anti-enumeration contract as the session
// endpoint's single 401).
const dummyPasswordHash = "$argon2id$v=19$m=19456,t=2,p=1$" +
	"AAAAAAAAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// errBadPHC is parsePHC's single rejection — every malformed shape is the
// same error because the only caller, verifyPassword, collapses all of them
// to false: a stored hash that is not argon2id PHC is a miss, never a hint.
var errBadPHC = errors.New("auth: not an argon2id PHC string")

// argonParams are the work factors embedded in a PHC string's m=…,t=…,p=…
// section; verifyPassword honors them so re-parameterized hashes coexist.
type argonParams struct {
	memoryKiB   uint32
	time        uint32
	parallelism uint32
}

// hashPassword derives the at-rest PHC string for a new password. A fresh
// 16-byte random salt per call is the whole point of the salt — it is not
// secret and travels inside the encoded string. Reserved for the
// set-password/provisioning flow (today nothing calls it outside tests;
// the column comment in migration 0006 says why writes stay
// qp_migrator-side until that flow lands).
func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt,
		argonTime, argonMemoryKiB, argonParallelism, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword reports whether password matches the stored PHC string.
// Two deliberate behaviors: the derived key length and the work factors come
// from the hash, not the package constants, so hashes written under older
// parameters keep verifying; and a malformed stored hash is simply false —
// it can only come from a writer outside this package, and the handler's
// uniform-401 contract treats it as a miss, not a server fault. The compare
// is subtle.ConstantTimeCompare so a partial key match tells nothing.
func verifyPassword(password, phc string) bool {
	params, salt, key, err := parsePHC(phc)
	if err != nil {
		return false
	}
	// #nosec G115 -- parallelism is range-checked to math.MaxUint8 in
	// parsePHC before this cast runs.
	derived := argon2.IDKey([]byte(password), salt,
		params.time, params.memoryKiB, uint8(params.parallelism), uint32(len(key)))
	return subtle.ConstantTimeCompare(derived, key) == 1
}

// parsePHC decodes "$argon2id$v=19$m=…,t=…,p=…$salt$key" — the only at-rest
// shape verifyPassword accepts. argon2i/argon2d PHC strings are rejected on
// the variant field, and the version must match this x/crypto build's
// argon2.Version so an unknown-format hash can never verify silently.
func parsePHC(phc string) (argonParams, []byte, []byte, error) {
	var p argonParams
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errBadPHC
	}
	var version uint32
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errBadPHC
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&p.memoryKiB, &p.time, &p.parallelism); err != nil {
		return p, nil, nil, errBadPHC
	}
	// Zero work factors and a parallelism past uint8 are not parameters a
	// writer of this format can have produced — a malformed stored hash is a
	// miss, and the bound keeps the uint8 cast provably safe.
	if p.memoryKiB == 0 || p.time == 0 || p.parallelism == 0 || p.parallelism > math.MaxUint8 {
		return p, nil, nil, errBadPHC
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, errBadPHC
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return p, nil, nil, errBadPHC
	}
	return p, salt, key, nil
}
