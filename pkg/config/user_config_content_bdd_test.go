package config_test

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("WithUserConfigContent", func() {
	var xdg, userPath string

	BeforeEach(func() {
		xdg = GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
		userPath = filepath.Join(xdg, "crosscodex", "config.yaml")
	})

	It("loads the given content as the user config instead of the file", func() {
		writeTestFile(userPath, "logging:\n  level: loud\n")
		cfg, err := config.NewLoader().Load(context.Background(),
			config.WithUserConfigContent([]byte("logging:\n  level: debug\n")))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Level).To(Equal("debug"))
	})

	It("loads the content when no user config file exists", func() {
		cfg, err := config.NewLoader().Load(context.Background(),
			config.WithUserConfigContent([]byte("logging:\n  format: json\n")))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Format).To(Equal("json"))
	})

	It("keeps the layers above the user config on top of the content", func() {
		writeTestFile(filepath.Join(xdg, "crosscodex", "conf.d", "50-local.yaml"), "logging:\n  level: error\n")
		cfg, err := config.NewLoader().Load(context.Background(),
			config.WithUserConfigContent([]byte("logging:\n  level: debug\n")))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Level).To(Equal("error"))
	})

	It("names the user config path in an unknown key error, at the content's line", func() {
		_, err := config.NewLoader().Load(context.Background(),
			config.WithUserConfigContent([]byte("logging:\n  levle: debug\n")))
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(err.Error()).To(ContainSubstring(userPath + ":2:3"))
	})

	It("names the user config path in a validation error", func() {
		_, err := config.NewLoader().Load(context.Background(),
			config.WithUserConfigContent([]byte("logging:\n  level: loud\n")))
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring(userPath))
	})

	It("treats empty content as an empty user config", func() {
		writeTestFile(userPath, "logging:\n  level: loud\n")
		cfg, err := config.NewLoader().Load(context.Background(), config.WithUserConfigContent([]byte{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Logging.Level).To(Equal("warn"))
	})
})
