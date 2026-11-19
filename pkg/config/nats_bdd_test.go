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

var _ = Describe("nats.streams.materialize_retention", func() {
	It("defaults to 168h", func() {
		_, cfg, err := loadConfigFile("")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.NATS.Streams.MaterializeRetention).To(Equal(168 * time.Hour))
	})

	It("accepts an override", func() {
		_, cfg, err := loadConfigFile("nats:\n  streams:\n    materialize_retention: 24h\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.NATS.Streams.MaterializeRetention).To(Equal(24 * time.Hour))
	})

	DescribeTable("rejects a non-positive value, saying what to set",
		func(retention time.Duration) {
			cfg := validConfig()
			cfg.NATS.Streams.MaterializeRetention = retention
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring("positive duration such as 168h")))
			expectOnlyFieldError(err, "nats.streams.materialize_retention")
		},
		Entry("zero", time.Duration(0)),
		Entry("negative", -time.Hour),
	)
})

var _ = Describe("nats.materialize", func() {
	It("defaults to 5 deliveries, a 10m ack wait, a 30s redelivery delay and a 2s barrier poll", func() {
		_, cfg, err := loadConfigFile("")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.NATS.Materialize).To(Equal(config.NATSMaterializeConfig{
			MaxDeliver:          config.DefaultNATSMaterializeMaxDeliver,
			AckWait:             config.DefaultNATSMaterializeAckWait,
			RedeliverDelay:      config.DefaultNATSMaterializeRedeliverDelay,
			BarrierPollInterval: config.DefaultNATSMaterializeBarrierPollInterval,
		}))
		Expect(config.DefaultNATSMaterializeBarrierPollInterval).To(Equal(2 * time.Second))
		Expect(config.DefaultNATSMaterializeMaxDeliver).To(Equal(5))
		Expect(config.DefaultNATSMaterializeAckWait).To(Equal(10 * time.Minute))
		Expect(config.DefaultNATSMaterializeRedeliverDelay).To(Equal(30 * time.Second))
	})

	It("accepts overrides of every field", func() {
		_, cfg, err := loadConfigFile("nats:\n  materialize:\n    max_deliver: 1\n    ack_wait: 30m\n    redeliver_delay: 2s\n    barrier_poll_interval: 1m\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.NATS.Materialize).To(Equal(config.NATSMaterializeConfig{
			MaxDeliver: 1, AckWait: 30 * time.Minute, RedeliverDelay: 2 * time.Second, BarrierPollInterval: time.Minute,
		}))
	})

	DescribeTable("rejects an invalid value under its path, saying what to set",
		func(path, advice string, mutate func(*config.NATSMaterializeConfig)) {
			cfg := validConfig()
			mutate(&cfg.NATS.Materialize)
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring(advice)))
			expectOnlyFieldError(err, path)
		},
		Entry("max_deliver 0", "nats.materialize.max_deliver", "at least 1, such as 5",
			func(m *config.NATSMaterializeConfig) { m.MaxDeliver = 0 }),
		Entry("max_deliver -1 (unlimited would retry a poison event forever)", "nats.materialize.max_deliver", "at least 1, such as 5",
			func(m *config.NATSMaterializeConfig) { m.MaxDeliver = -1 }),
		Entry("ack_wait 0", "nats.materialize.ack_wait", "positive duration such as 10m",
			func(m *config.NATSMaterializeConfig) { m.AckWait = 0 }),
		Entry("ack_wait negative", "nats.materialize.ack_wait", "positive duration such as 10m",
			func(m *config.NATSMaterializeConfig) { m.AckWait = -time.Minute }),
		Entry("redeliver_delay 0", "nats.materialize.redeliver_delay", "positive duration such as 30s",
			func(m *config.NATSMaterializeConfig) { m.RedeliverDelay = 0 }),
		Entry("redeliver_delay negative", "nats.materialize.redeliver_delay", "positive duration such as 30s",
			func(m *config.NATSMaterializeConfig) { m.RedeliverDelay = -time.Second }),
		Entry("barrier_poll_interval 0", "nats.materialize.barrier_poll_interval", "positive duration such as 2s",
			func(m *config.NATSMaterializeConfig) { m.BarrierPollInterval = 0 }),
		Entry("barrier_poll_interval negative", "nats.materialize.barrier_poll_interval", "positive duration such as 2s",
			func(m *config.NATSMaterializeConfig) { m.BarrierPollInterval = -time.Second }),
	)

	It("rejects max_deliver 0 from a config file under its path", func() {
		_, _, err := loadConfigFile("nats:\n  materialize:\n    max_deliver: 0\n")
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring("nats.materialize.max_deliver"))
	})
})
