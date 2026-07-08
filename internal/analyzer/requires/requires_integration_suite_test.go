//go:build integration

package requires_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRequiresIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Requires Analyzer Integration BDD Suite")
}
