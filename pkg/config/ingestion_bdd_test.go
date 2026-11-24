package config_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("IngestionConfig", func() {
	It("is unconfigured by default, with the documented tuning defaults", func() {
		_, cfg, err := loadConfigFile("{}\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Ingestion).To(Equal(config.IngestionConfig{
			Address:          "",
			PollInterval:     2 * time.Second,
			ConvertTimeout:   10 * time.Minute,
			MaxDocumentBytes: 50 << 20,
		}))
	})

	It("accepts a valid override of every field", func() {
		_, cfg, err := loadConfigFile(`
ingestion:
  address: ingestion.internal:8090
  poll_interval: 500ms
  convert_timeout: 30m
  max_document_bytes: 1048576
`)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Ingestion).To(Equal(config.IngestionConfig{
			Address:          "ingestion.internal:8090",
			PollInterval:     500 * time.Millisecond,
			ConvertTimeout:   30 * time.Minute,
			MaxDocumentBytes: 1 << 20,
		}))
	})

	It("accepts convert_timeout equal to poll_interval", func() {
		_, _, err := loadConfigFile("ingestion:\n  poll_interval: 5s\n  convert_timeout: 5s\n")
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("rejects an invalid value naming the field and a valid example",
		func(yamlBody, path, example string) {
			_, _, err := loadConfigFile(yamlBody)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring(path))
			Expect(err.Error()).To(ContainSubstring(example))
			expectOnlyFieldError(err, path)
		},
		Entry("zero poll_interval", "ingestion:\n  poll_interval: 0s\n", "ingestion.poll_interval", "2s"),
		Entry("negative poll_interval", "ingestion:\n  poll_interval: -1s\n", "ingestion.poll_interval", "2s"),
		Entry("convert_timeout below poll_interval", "ingestion:\n  poll_interval: 10s\n  convert_timeout: 5s\n", "ingestion.convert_timeout", "10m"),
		Entry("zero max_document_bytes", "ingestion:\n  max_document_bytes: 0\n", "ingestion.max_document_bytes", "52428800"),
		Entry("negative max_document_bytes", "ingestion:\n  max_document_bytes: -1\n", "ingestion.max_document_bytes", "52428800"),
		Entry("address without a port", "ingestion:\n  address: ingestion.internal\n", "ingestion.address", "ingestion.internal:8090"),
		Entry("address with an empty port", "ingestion:\n  address: \"ingestion.internal:\"\n", "ingestion.address", "ingestion.internal:8090"),
		Entry("address that is a URL", "ingestion:\n  address: http://ingestion.internal:8090\n", "ingestion.address", "ingestion.internal:8090"),
	)

	DescribeTable("is global-only: a tenant override of any field is rejected as an unknown key",
		func(field string) {
			_, _, err := loadConfigFile("ingestion:\n  tenant_overrides:\n    acme:\n      " + field + "\n")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant_overrides"))
		},
		Entry("address", "address: other.internal:8090"),
		Entry("poll_interval", "poll_interval: 1s"),
		Entry("convert_timeout", "convert_timeout: 1m"),
		Entry("max_document_bytes", "max_document_bytes: 1024"),
	)
})
