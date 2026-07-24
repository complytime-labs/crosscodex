package main

import (
	"bytes"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Results Commands", func() {
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

	Describe("results summary", func() {
		It("requires a job-id argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "summary"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job-id"))
		})

		It("includes usage examples in error message", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "summary"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("results summary"))
		})

		It("supports 'sum' alias", func() {
			cmd := newRootCmd()
			resultsCmd, _, err := cmd.Find([]string{"results", "sum"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resultsCmd.Name()).To(Equal("summary"))
		})
	})

	Describe("results export", func() {
		It("requires a job-id argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "export"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job-id"))
		})

		It("has --format flag", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "export", "--help"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).To(ContainSubstring("--format"))
		})

		It("supports 'dump' alias", func() {
			cmd := newRootCmd()
			resultsCmd, _, err := cmd.Find([]string{"results", "dump"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resultsCmd.Name()).To(Equal("export"))
		})
	})

	Describe("results query", func() {
		It("requires a cypher query argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "query"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cypher"))
		})

		It("includes usage examples in error message", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "query"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("MATCH"))
		})
	})

	Describe("results debug", func() {
		It("requires job-id and control-id arguments", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "debug"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job-id"))
		})

		It("requires control-id when only job-id provided", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "debug", "job123"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("control-id"))
		})
	})

	Describe("results review", func() {
		It("requires a job-id argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "review"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job-id"))
		})
	})

	Describe("results verify", func() {
		It("requires a bundle-file argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bundle"))
		})

		It("includes usage examples in error message", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("results verify"))
		})

		It("validates valid JSON bundle without public key", func() {
			tmpFile, err := os.CreateTemp("", "test-bundle-*.json")
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(tmpFile.Name())
			Expect(os.WriteFile(tmpFile.Name(), []byte(`{"payloadType": "application/vnd.in-toto+json", "payload": "test"}`), 0o644)).To(Succeed())
			tmpFile.Close()

			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify", tmpFile.Name()})
			err = cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).To(ContainSubstring("Structure valid"))
		})

		It("rejects invalid JSON bundle", func() {
			tmpFile, err := os.CreateTemp("", "test-bundle-*.json")
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(tmpFile.Name())
			Expect(os.WriteFile(tmpFile.Name(), []byte("not json"), 0o644)).To(Succeed())
			tmpFile.Close()

			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify", tmpFile.Name()})
			err = cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not valid JSON"))
		})

		It("errors when bundle file does not exist", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify", "/nonexistent/bundle.json"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("read bundle file"))
		})
	})
})
