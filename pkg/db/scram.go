package db

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// scramIterations is PostgreSQL's default scram_iterations.
const scramIterations = 4096

// SCRAMVerifier returns the SCRAM-SHA-256 verifier PostgreSQL stores for
// password (RFC 5802, RFC 7677), in the form ALTER ROLE ... PASSWORD
// accepts: SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>. Sending
// the verifier instead of the password keeps the plaintext out of the server
// and its logs, as psql's \password does. password must be printable ASCII,
// for which SASLprep (applied by the server at login) is the identity. salt
// must be at least 16 bytes; callers pass crypto/rand output.
func SCRAMVerifier(password string, salt []byte) (string, error) {
	if password == "" {
		return "", errors.New("scram verifier: refusing an empty password")
	}
	for _, r := range password {
		if r < 0x21 || r > 0x7e {
			return "", errors.New("scram verifier: the password must be printable ASCII without spaces; generate one with `openssl rand -hex 24`")
		}
	}
	if len(salt) < 16 {
		return "", fmt.Errorf("scram verifier: the salt is %d bytes; it must be at least 16 bytes", len(salt))
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("scram verifier: %w", err)
	}
	clientKey := scramHMAC(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := scramHMAC(salted, "Server Key")
	enc := base64.StdEncoding
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", scramIterations,
		enc.EncodeToString(salt), enc.EncodeToString(storedKey[:]), enc.EncodeToString(serverKey)), nil
}

func scramHMAC(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
