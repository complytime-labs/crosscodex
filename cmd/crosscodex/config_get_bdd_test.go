package main

import (
	"bytes"
	"encoding/json"

	"github.com/complytime-labs/crosscodex/pkg/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("config get", func() {
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

	Describe("getConfigValue", func() {
		var cfg *config.Config

		BeforeEach(func() {
			cfg = &config.Config{
				Database: config.DatabaseConfig{
					DSN:      "postgres://user:pass@localhost:5432/db",
					GraphDSN: "postgres://graph@localhost:5432/db",
					MaxConns: 10,
				},
				CLI: config.CLISettings{
					Output:   "json",
					Endpoint: "localhost:50051",
				},
				TLS: config.TLSConfig{
					Mode: "mutual",
				},
				LLM: config.LLMConfig{
					DefaultModel: "gpt-4",
					MaxRetries:   3,
				},
			}
		})

		It("returns a top-level subtree", func() {
			val, err := getConfigValue(cfg, "database")
			Expect(err).NotTo(HaveOccurred())
			db, ok := val.(config.DatabaseConfig)
			Expect(ok).To(BeTrue())
			Expect(db.DSN).To(Equal("postgres://user:pass@localhost:5432/db"))
		})

		It("returns a nested scalar via dot-notation", func() {
			val, err := getConfigValue(cfg, "database.dsn")
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal("postgres://user:pass@localhost:5432/db"))
		})

		It("returns a nested integer scalar", func() {
			val, err := getConfigValue(cfg, "database.max_conns")
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal(10))
		})

		It("returns an error for a non-existent top-level key", func() {
			_, err := getConfigValue(cfg, "nonexistent")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`key "nonexistent" not found`))
		})

		It("returns an error for a non-existent nested key", func() {
			_, err := getConfigValue(cfg, "database.nonexistent")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`key "database.nonexistent" not found`))
		})

		It("returns an error for deep non-existent path", func() {
			_, err := getConfigValue(cfg, "foo.bar.baz")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`key "foo.bar.baz" not found`))
		})

		It("returns an error when traversing through a scalar", func() {
			_, err := getConfigValue(cfg, "database.dsn.extra")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})

	Describe("command integration", func() {
		It("requires a key argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"config", "get"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("key"))
		})

		It("returns a subtree as YAML", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"config", "get", "database"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).To(ContainSubstring("dsn:"))
			Expect(output).To(ContainSubstring("max_conns:"))
		})

		It("returns a scalar value directly", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"config", "get", "cli.output"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).NotTo(BeEmpty())
			Expect(output).NotTo(ContainSubstring("output:"))
		})

		It("returns JSON when --json is set", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"config", "get", "database", "--json"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			var result map[string]any
			Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
			Expect(result).To(HaveKey("dsn"))
			Expect(result).To(HaveKey("max_conns"))
		})

		It("errors on a non-existent key", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"config", "get", "foo.bar.baz"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})
})
