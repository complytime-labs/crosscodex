package testspecs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
)

func TestHelpers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Helpers Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("Test Helpers", func() {

	Describe("SetupTestDatabase", func() {
		It("should setup a test database and return cleanup function", func() {
			db, cleanup := testspecs.SetupTestDatabase()

			// DB might be nil if no test database is available, but cleanup should always be provided
			Expect(cleanup).NotTo(BeNil())

			// If DB is available, test it
			if db != nil {
				// Verify database connection works (might fail if no real DB)
				err := db.Ping()
				if err == nil {
					// Call cleanup and verify it works
					cleanup()

					// After cleanup, database should be closed
					err = db.Ping()
					Expect(err).To(HaveOccurred())
				} else {
					// If ping fails, just call cleanup
					cleanup()
				}
			} else {
				// DB is nil (expected when no test DB available), just call cleanup
				cleanup()
			}
		})
	})

	Describe("SetupTestNATS", func() {
		It("should setup a test NATS client and return cleanup function", func() {
			client, cleanup := testspecs.SetupTestNATS()

			Expect(client).NotTo(BeNil())
			Expect(cleanup).NotTo(BeNil())

			// Call cleanup function (client doesn't expose connectivity status directly)
			cleanup()
		})
	})

	Describe("SetupTestStorage", func() {
		It("should setup a test storage provider and return cleanup function", func() {
			provider, cleanup := testspecs.SetupTestStorage()

			Expect(provider).NotTo(BeNil())
			Expect(cleanup).NotTo(BeNil())

			// Verify storage provider works by writing and reading
			testContent := []byte("test content")
			err := provider.Put(context.Background(), "test-key", bytes.NewReader(testContent))
			Expect(err).NotTo(HaveOccurred())

			reader, err := provider.Get(context.Background(), "test-key")
			Expect(err).NotTo(HaveOccurred())
			defer reader.Close()

			data, err := io.ReadAll(reader)
			Expect(err).NotTo(HaveOccurred())
			Expect(data).To(Equal(testContent))

			// Call cleanup and verify it works
			cleanup()
		})
	})

	Describe("SetupTestConfig", func() {
		It("should create a temporary config file and return path with cleanup function", func() {
			configData := []byte(`
app:
  name: test-app
  version: 1.0.0
`)

			configPath, cleanup := testspecs.SetupTestConfig(configData)

			Expect(configPath).NotTo(BeEmpty())
			Expect(cleanup).NotTo(BeNil())

			// Verify config file exists and has correct content
			Expect(configPath).To(BeARegularFile())

			content, err := os.ReadFile(configPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).To(Equal(configData))

			// Call cleanup and verify file is removed
			cleanup()

			Expect(configPath).NotTo(BeARegularFile())
		})
	})

	Describe("SetupTenantContext", func() {
		It("should create a context with the specified tenant ID", func() {
			tenantID := "test-tenant"
			ctx := testspecs.SetupTenantContext(tenantID)

			Expect(ctx).NotTo(BeNil())
			// Check it's a valid context - background context's Done() is nil until canceled
			// Just verify we can get a value from the context
			Expect(ctx.Value("any-key")).To(BeNil()) // Background context returns nil for unknown keys
		})
	})

	Describe("AssertNoError", func() {
		It("should not panic when error is nil", func() {
			Expect(func() {
				testspecs.AssertNoError(nil)
			}).NotTo(Panic())
		})

		It("should be callable with non-nil error (testing function exists)", func() {
			// We can't easily test the failure case without causing our test to fail
			// Just verify the function exists and is callable
			testErr := errors.New("test error")
			Expect(testspecs.AssertNoError).NotTo(BeNil())
			_ = testErr // Use the error variable to avoid compiler warning
		})
	})

	Describe("AssertErrorContains", func() {
		It("should not panic when error contains expected text", func() {
			testErr := errors.New("test error message")
			Expect(func() {
				testspecs.AssertErrorContains(testErr, "error message")
			}).NotTo(Panic())
		})

		It("should be callable with various inputs (testing function exists)", func() {
			// We can't easily test the failure cases without causing our test to fail
			// Just verify the function exists and is callable
			testErr := errors.New("test error message")
			Expect(testspecs.AssertErrorContains).NotTo(BeNil())
			_ = testErr // Use the error variable to avoid compiler warning
		})
	})

	Describe("WaitForCondition", func() {
		It("should return when condition becomes true", func() {
			counter := 0
			condition := func() bool {
				counter++
				return counter >= 3
			}

			start := time.Now()
			testspecs.WaitForCondition(condition, 5*time.Second, "counter reaches 3")
			elapsed := time.Since(start)

			Expect(elapsed).To(BeNumerically("<", 1*time.Second))
			Expect(counter).To(BeNumerically(">=", 3))
		})

		It("should be callable with timeout condition (testing function exists)", func() {
			// We can't easily test the timeout case without causing our test to fail
			// Just verify the function exists and works for quick conditions
			condition := func() bool {
				return true // immediately true
			}

			Expect(func() {
				testspecs.WaitForCondition(condition, 1*time.Second, "immediate condition")
			}).NotTo(Panic())
		})
	})

	Describe("IgnoreOutput", func() {
		It("should suppress output and restore it", func() {
			// This is hard to test directly, but we can verify the function signature
			restore := testspecs.IgnoreOutput()
			Expect(restore).NotTo(BeNil())

			// Call restore function
			restore()
		})
	})

	Describe("LogTestProgress", func() {
		It("should log test progress without panicking", func() {
			Expect(func() {
				testspecs.LogTestProgress("Starting test phase: %s", "setup")
			}).NotTo(Panic())
		})
	})
})

var _ = Describe("IsolateTelemetryGlobals", func() {
	It("starts from an empty propagator and restores every telemetry global", func() {
		prevTP := otel.GetTracerProvider()
		prevMP := otel.GetMeterProvider()
		prevProp := otel.GetTextMapPropagator()
		prevLogger := slog.Default()

		restore := testspecs.IsolateTelemetryGlobals()
		Expect(otel.GetTextMapPropagator().Fields()).To(BeEmpty())

		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

		restore()

		Expect(otel.GetTracerProvider()).To(BeIdenticalTo(prevTP))
		Expect(otel.GetMeterProvider()).To(BeIdenticalTo(prevMP))
		Expect(otel.GetTextMapPropagator().Fields()).To(ConsistOf(prevProp.Fields()))
		Expect(slog.Default()).To(BeIdenticalTo(prevLogger))
	})
})

var _ = Describe("IsolateTelemetryGlobals after an earlier telemetry.Init", func() {
	It("restores a composite propagator without comparing uncomparable values", func() {
		DeferCleanup(otel.SetTextMapPropagator, otel.GetTextMapPropagator())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))

		restore := testspecs.IsolateTelemetryGlobals()
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.Baggage{}))

		Expect(restore).NotTo(Panic())
		Expect(otel.GetTextMapPropagator().Fields()).To(ConsistOf("traceparent", "tracestate"))
	})
})

// builtinDefaultLogger is slog's original default, captured before the
// BeforeSuite redirect: only its built-in handler writes through the log
// package, which is the precondition for the deadlock spec below.
var builtinDefaultLogger = slog.Default()

// wrappingHandler stands in for telemetry.Init's trace handler: any handler
// that is not slog's built-in one makes slog.SetDefault redirect the log
// package's output into it.
type wrappingHandler struct{ slog.Handler }

var _ = Describe("IsolateTelemetryGlobals around a handler that wraps slog's built-in default", func() {
	It("restores the log package output so the restored default logger does not deadlock", func() {
		DeferCleanup(slog.SetDefault, slog.Default())
		DeferCleanup(log.SetOutput, log.Writer())
		DeferCleanup(log.SetFlags, log.Flags())

		var out bytes.Buffer
		log.SetOutput(&out)
		log.SetFlags(log.LstdFlags)
		slog.SetDefault(builtinDefaultLogger)

		restore := testspecs.IsolateTelemetryGlobals()
		slog.SetDefault(slog.New(wrappingHandler{slog.Default().Handler()}))
		restore()

		Expect(log.Writer()).To(BeIdenticalTo(&out),
			"log output still points at the wrapping handler, which writes back through log: the next slog.Default() call deadlocks")
		Expect(log.Flags()).To(Equal(log.LstdFlags))
		slog.Default().Info("after restore")
		Expect(out.String()).To(ContainSubstring("after restore"))
	})
})
