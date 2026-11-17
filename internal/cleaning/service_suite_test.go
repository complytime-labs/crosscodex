package cleaning_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCleaningService(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Edge Cleaning Service Suite")
}
