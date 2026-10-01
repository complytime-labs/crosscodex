package main

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
)

var _ = Describe("effectiveRole", func() {
	It("prefers the CLI flag over the loaded config value", func() {
		Expect(effectiveRole("worker", "all")).To(Equal("worker"))
	})

	It("falls back to the config value when the flag is unset", func() {
		Expect(effectiveRole("", "graph")).To(Equal("graph"))
	})
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
