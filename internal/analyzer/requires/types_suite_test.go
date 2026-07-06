package requires_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRequiresBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Requires Analyzer BDD Suite")
}
