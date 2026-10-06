package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("effectiveRole", func() {
	It("prefers the CLI flag over the loaded config value", func() {
		Expect(effectiveRole("worker", "all")).To(Equal("worker"))
	})

	It("falls back to the config value when the flag is unset", func() {
		Expect(effectiveRole("", "graph")).To(Equal("graph"))
	})
})

var _ = Describe("newLogger", func() {
	DescribeTable("writes records in the configured format",
		func(format string, matcher OmegaMatcher) {
			var buf bytes.Buffer
			newLogger(&buf, config.LoggingConfig{Level: "info", Format: format}).Info("hello")
			Expect(buf.String()).To(matcher)
		},
		Entry("json", "json", MatchRegexp(`^\{.*"level":"INFO","msg":"hello"\}\n$`)),
		Entry("text", "text", MatchRegexp(`^time=\S+ level=INFO msg=hello\n$`)),
		Entry("empty falls back to text", "", MatchRegexp(`^time=\S+ level=INFO msg=hello\n$`)),
	)

	DescribeTable("enables the configured level and nothing below it",
		func(level string, want slog.Level) {
			logger := newLogger(io.Discard, config.LoggingConfig{Level: level, Format: "text"})
			Expect(logger.Enabled(context.Background(), want)).To(BeTrue())
			Expect(logger.Enabled(context.Background(), want-1)).To(BeFalse())
		},
		Entry("debug", "debug", slog.LevelDebug),
		Entry("info", "info", slog.LevelInfo),
		Entry("warn", "warn", slog.LevelWarn),
		Entry("error", "error", slog.LevelError),
		Entry("empty falls back to warn", "", slog.LevelWarn),
	)
})

var _ = Describe("run telemetry initialization", func() {
	// runWithEnv runs the daemon with the graph role and returns the exit code
	// and stderr. Every value the specs depend on is pinned through
	// CROSSCODEX_* variables, which outrank the system, user and ambient
	// configuration layers. The database DSN points at a closed port so
	// bootstrap fails fast instead of starting a real daemon.
	runWithEnv := func(env map[string]string) (int, string) {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_DATABASE_DSN", "postgres://u:p@127.0.0.1:1/x?sslmode=disable")
		for k, v := range env {
			GinkgoT().Setenv(k, v)
		}

		stderrFile, err := os.CreateTemp(GinkgoT().TempDir(), "stderr")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(stderrFile.Close)
		origStderr := os.Stderr
		os.Stderr = stderrFile
		DeferCleanup(func() { os.Stderr = origStderr })

		code := run("graph")

		os.Stderr = origStderr
		stderr, err := os.ReadFile(stderrFile.Name())
		Expect(err).NotTo(HaveOccurred())
		return code, string(stderr)
	}

	BeforeEach(func() {
		DeferCleanup(testspecs.IsolateTelemetryGlobals())
	})

	It("initializes telemetry before bootstrapping resources", func() {
		code, stderr := runWithEnv(map[string]string{
			"CROSSCODEX_OBSERVABILITY_ENDPOINT":         "",
			"CROSSCODEX_OBSERVABILITY_TRACING_ENDPOINT": "",
			"CROSSCODEX_OBSERVABILITY_METRICS_ENDPOINT": "",
		})

		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("bootstrap:"))
		Expect(otel.GetTextMapPropagator().Fields()).To(ContainElement("traceparent"),
			"telemetry.Init must run before bootstrap so resources see the real providers")
	})

	// slog's built-in handler writes through the log package, and
	// slog.SetDefault points log's output at the new default handler. Wrapping
	// the built-in handler in Init's trace handler therefore loops back into
	// log and deadlocks on its mutex at the first log call.
	It("leaves a default logger that logs without deadlocking", func() {
		code, _ := runWithEnv(map[string]string{
			"CROSSCODEX_OBSERVABILITY_ENDPOINT":         "",
			"CROSSCODEX_OBSERVABILITY_TRACING_ENDPOINT": "",
			"CROSSCODEX_OBSERVABILITY_METRICS_ENDPOINT": "",
		})
		Expect(code).To(Equal(1))

		logged := make(chan struct{})
		go func() {
			defer close(logged)
			slog.Error("logged after telemetry.Init")
		}()
		Eventually(logged).WithTimeout(5*time.Second).Should(BeClosed(),
			"the first log call after telemetry.Init must not deadlock")
	})

	It("refuses to start with an actionable error when observability config is invalid", func() {
		code, stderr := runWithEnv(map[string]string{
			"CROSSCODEX_OBSERVABILITY_ENDPOINT":         "localhost:4317",
			"CROSSCODEX_OBSERVABILITY_TRACING_ENDPOINT": "",
			"CROSSCODEX_OBSERVABILITY_TRACING_PROTOCOL": "carrier-pigeon",
		})

		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("initialize telemetry"))
		Expect(stderr).To(ContainSubstring(`unsupported tracing protocol "carrier-pigeon"`))
		Expect(stderr).NotTo(ContainSubstring("bootstrap:"),
			"an invalid observability config must stop the daemon before it touches the database")
		Expect(otel.GetTextMapPropagator().Fields()).To(BeEmpty())
	})
})
