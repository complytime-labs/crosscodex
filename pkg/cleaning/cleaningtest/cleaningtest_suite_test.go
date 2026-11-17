package cleaningtest_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCleaningTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CleaningTest Suite")
}
