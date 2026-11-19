package config_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("nats.publish_ack_timeout", func() {
	It("defaults to 10s", func() {
		_, cfg, err := loadConfigFile("")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.NATS.PublishAckTimeout).To(Equal(10 * time.Second))
	})

	It("accepts an override", func() {
		_, cfg, err := loadConfigFile("nats:\n  publish_ack_timeout: 45s\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.NATS.PublishAckTimeout).To(Equal(45 * time.Second))
	})

	DescribeTable("rejects a non-positive value, saying what to set",
		func(timeout time.Duration) {
			cfg := validConfig()
			cfg.NATS.PublishAckTimeout = timeout
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring("positive duration such as 10s")))
			expectOnlyFieldError(err, "nats.publish_ack_timeout")
		},
		Entry("zero", time.Duration(0)),
		Entry("negative", -time.Second),
	)

	It("rejects 0s from a config file under its path", func() {
		_, _, err := loadConfigFile("nats:\n  publish_ack_timeout: 0s\n")
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring("nats.publish_ack_timeout"))
	})
})
