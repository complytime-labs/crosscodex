package testspecs

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// TestCleanup represents a cleanup function that should be called after a test
type TestCleanup func()

// SetupTestDatabase creates a test database connection with proper cleanup
func SetupTestDatabase() (*sql.DB, TestCleanup) {
	// Check for test database environment variable, skip if not available
	testDSN := os.Getenv("TEST_DATABASE_DSN")
	if testDSN == "" {
		// Use a mock DSN for unit tests when no real database is available
		testDSN = "postgres://test:test@localhost/test?sslmode=disable" // DevSkim: ignore DS162092 — test fixture
	}

	// Create database pool configuration
	poolCfg := db.PoolConfig{
		DSN:          testDSN,
		MaxOpenConns: 10,
		MaxIdleConns: 5,
	}

	// Try to create database pool
	pool, err := db.NewPool(poolCfg)
	if err != nil {
		// If pool creation fails, still return a valid cleanup function
		return nil, func() {}
	}

	// Try to create a simple SQL connection for compatibility
	sqlDB, err := sql.Open("pgx", testDSN)
	if err != nil {
		// If SQL connection fails, close pool and return nil
		pool.Close()
		return nil, func() {}
	}

	cleanup := func() {
		if sqlDB != nil {
			sqlDB.Close()
		}
		if pool != nil {
			pool.Close()
		}
	}

	return sqlDB, cleanup
}

// SetupTestNATS creates a test NATS client with proper cleanup
// Returns the client itself for testing, not the underlying connection
func SetupTestNATS() (natsbus.Client, TestCleanup) {
	// Use embedded NATS for testing
	cfg := config.NATSConfig{
		URL: "", // Empty URL means embedded mode
		Embedded: config.NATSEmbeddedConfig{
			StoreDir: "", // Will use temp directory
		},
		Streams: config.NATSStreamsConfig{
			AuditLLMRetention:    90 * 24 * time.Hour, // 90 days
			AuditEventsRetention: 30 * 24 * time.Hour, // 30 days
		},
	}

	// Create NATS client
	client, err := natsbus.New(cfg)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	cleanup := func() {
		if client != nil {
			client.Close()
		}
	}

	return client, cleanup
}

// SetupTestStorage creates a test storage provider with proper cleanup
func SetupTestStorage() (storage.Provider, TestCleanup) {
	// Create temporary directory for test storage
	tempDir, err := os.MkdirTemp("", "crosscodex_test_storage_*")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	// Create local storage provider pointing to temp directory
	// Use a test tenant ID
	testTenantID := "test-tenant"
	provider, err := storage.NewLocal(tempDir, testTenantID)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	cleanup := func() {
		if provider != nil {
			provider.Close()
		}
		// Remove the temporary directory and all its contents
		os.RemoveAll(tempDir)
	}

	return provider, cleanup
}

// SetupTestConfig creates a temporary config file and returns its path with cleanup
func SetupTestConfig(configData []byte) (string, TestCleanup) {
	// Create temporary file for config
	tempFile, err := os.CreateTemp("", "crosscodex_test_config_*.yml")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	configPath := tempFile.Name()

	// Write config data to file
	_, err = tempFile.Write(configData)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	err = tempFile.Close()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	cleanup := func() {
		os.Remove(configPath)
	}

	return configPath, cleanup
}

// SetupTenantContext creates a context with the specified tenant ID
func SetupTenantContext(tenantID string) context.Context {
	return tenant.WithTenant(context.Background(), tenantID)
}

// WaitForCondition waits for a condition to become true with timeout
func WaitForCondition(condition func() bool, timeout time.Duration, description string) {
	start := time.Now()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		if condition() {
			return
		}
		if time.Since(start) >= timeout {
			ExpectWithOffset(1, true).To(BeFalse(), fmt.Sprintf("Timeout waiting for condition: %s", description))
			return
		}
	}
}

// AssertNoError asserts that the error is nil using proper offset
func AssertNoError(err error) {
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

// AssertErrorContains asserts that the error contains the expected text
func AssertErrorContains(err error, expectedText string) {
	ExpectWithOffset(1, err).To(HaveOccurred())
	ExpectWithOffset(1, err.Error()).To(ContainSubstring(expectedText))
}

// IgnoreOutput suppresses output to stdout/stderr and returns a restore function
func IgnoreOutput() (restore func()) {
	originalStdout := os.Stdout
	originalStderr := os.Stderr

	// Create a pipe to discard output
	_, w, err := os.Pipe()
	if err != nil {
		// If we can't create a pipe, just return a no-op
		return func() {}
	}

	// Redirect stdout and stderr to the write end of the pipe
	os.Stdout = w
	os.Stderr = w

	restore = func() {
		// Close the write end
		w.Close()

		// Restore original stdout and stderr
		os.Stdout = originalStdout
		os.Stderr = originalStderr
	}

	return restore
}

// LogTestProgress logs test progress messages using Ginkgo's logging
func LogTestProgress(message string, args ...interface{}) {
	formattedMessage := fmt.Sprintf(message, args...)
	GinkgoWriter.Printf("[TEST PROGRESS] %s\n", formattedMessage)
}

// RedirectLogsToGinkgo sets slog's default logger to write to GinkgoWriter.
// Ginkgo captures this output and only displays it when a spec fails,
// keeping the normal test output clean. Returns a restore function that
// reinstates the previous default logger.
//
// Call in BeforeSuite or BeforeEach:
//
//	var restoreLogs func()
//	BeforeSuite(func() { restoreLogs = testspecs.RedirectLogsToGinkgo() })
//	AfterSuite(func() { restoreLogs() })
func RedirectLogsToGinkgo() (restore func()) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
	return func() { slog.SetDefault(prev) }
}

// GinkgoLogger returns an *slog.Logger that writes to GinkgoWriter.
// Use this when injecting a logger into a component under test so its
// output is captured by Ginkgo and only shown on failure.
func GinkgoLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(GinkgoWriter, nil))
}
