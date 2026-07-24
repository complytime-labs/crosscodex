package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
)

var _ = Describe("Output Infrastructure", func() {
	var (
		cmd    *cobra.Command
		stdout bytes.Buffer
	)

	BeforeEach(func() {
		stdout.Reset()
		cmd = &cobra.Command{Use: "test"}
		cmd.SetOut(&stdout)
		cmd.Flags().Bool("json", false, "")
		cmd.Flags().Bool("plain", false, "")
		cmd.Flags().Bool("no-color", false, "")
	})

	Describe("emit", func() {
		It("writes human output by default", func() {
			err := emit(cmd, func(w io.Writer, color bool) {
				fmt.Fprint(w, "hello human")
			}, map[string]string{"msg": "hello"})
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout.String()).To(Equal("hello human"))
		})

		It("writes JSON when --json is set", func() {
			Expect(cmd.Flags().Set("json", "true")).To(Succeed())
			err := emit(cmd, func(w io.Writer, color bool) {
				fmt.Fprint(w, "should not appear")
			}, map[string]string{"msg": "hello"})
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout.String()).To(ContainSubstring(`"msg": "hello"`))
		})

		It("writes plain output when --plain is set", func() {
			Expect(cmd.Flags().Set("plain", "true")).To(Succeed())
			err := emit(cmd, func(w io.Writer, color bool) {
				Expect(color).To(BeFalse())
				fmt.Fprint(w, "plain output")
			}, map[string]string{"msg": "hello"})
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout.String()).To(Equal("plain output"))
		})
	})

	Describe("useColor", func() {
		It("returns false when --no-color is set", func() {
			Expect(cmd.Flags().Set("no-color", "true")).To(Succeed())
			Expect(useColor(cmd)).To(BeFalse())
		})

		It("returns false when --json is set", func() {
			Expect(cmd.Flags().Set("json", "true")).To(Succeed())
			Expect(useColor(cmd)).To(BeFalse())
		})

		It("returns false when --plain is set", func() {
			Expect(cmd.Flags().Set("plain", "true")).To(Succeed())
			Expect(useColor(cmd)).To(BeFalse())
		})

		It("returns false when NO_COLOR is set", func() {
			os.Setenv("NO_COLOR", "")
			defer os.Unsetenv("NO_COLOR")
			Expect(useColor(cmd)).To(BeFalse())
		})

		It("returns false when CROSSCODEX_COLOR is not 1", func() {
			os.Setenv("CROSSCODEX_COLOR", "0")
			defer os.Unsetenv("CROSSCODEX_COLOR")
			Expect(useColor(cmd)).To(BeFalse())
		})
	})

	Describe("formatCLIError", func() {
		It("formats plain text error", func() {
			msg := formatCLIError(fmt.Errorf("something broke"), false)
			Expect(msg).To(Equal("error: something broke"))
		})

		It("formats JSON error", func() {
			msg := formatCLIError(fmt.Errorf("something broke"), true)
			Expect(msg).To(ContainSubstring(`"error"`))
			Expect(msg).To(ContainSubstring("something broke"))
		})
	})

	Describe("writeJSON", func() {
		It("writes indented JSON to stdout", func() {
			err := writeJSON(cmd, map[string]string{"key": "value"})
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout.String()).To(ContainSubstring("  \"key\": \"value\""))
			Expect(stdout.String()).To(HaveSuffix("\n"))
		})
	})
})
