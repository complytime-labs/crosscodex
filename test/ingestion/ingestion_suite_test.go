//go:build integration_ingestion

package ingestion_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestIngestionIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Ingestion Integration Suite")
}
