package dbtest

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

// Environment variables carrying role passwords for one integration run.
// .taskfiles/test/integration.yml (app_user, purge_user) and
// .taskfiles/test/e2e.yml (graph_user) generate a fresh random hex value for
// each on every task run, so every package and parallel Ginkgo node in that
// run agrees on it.
const (
	AppUserPasswordEnv   = "TEST_APP_USER_PASSWORD"
	PurgeUserPasswordEnv = "TEST_PURGE_USER_PASSWORD"
	GraphUserPasswordEnv = "TEST_GRAPH_USER_PASSWORD"
)

// NewRolePassword returns a random 32-character lowercase hex password.
func NewRolePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate role password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// RolePassword returns the role password in the environment variable env.
// It refuses an unset or non-hex value, because AlterRolePasswordSQL
// interpolates the password.
func RolePassword(env string) (string, error) {
	pw := os.Getenv(env)
	if pw == "" {
		return "", fmt.Errorf("%s is not set: run the suites through task test:integration:<name> or task test:e2e:venom, which generate a role password for each run", env)
	}
	if err := checkHex(pw); err != nil {
		return "", fmt.Errorf("%s: %w", env, err)
	}
	return pw, nil
}

// AlterRolePasswordSQL returns the statement that sets role's password.
// ALTER ROLE takes no bound parameters, so both values are interpolated:
// role is quoted with pgx.Identifier, and password must be hex, which
// cannot contain a quote.
func AlterRolePasswordSQL(role, password string) (string, error) {
	if err := checkHex(password); err != nil {
		return "", fmt.Errorf("set %s password: %w", role, err)
	}
	return fmt.Sprintf("ALTER ROLE %s WITH PASSWORD '%s'", pgx.Identifier{role}.Sanitize(), password), nil
}

func checkHex(password string) error {
	if password == "" {
		return errors.New("refusing an empty password; generate one with dbtest.NewRolePassword")
	}
	if _, err := hex.DecodeString(password); err != nil {
		return errors.New("refusing a non-hex password; generate one with dbtest.NewRolePassword")
	}
	return nil
}
