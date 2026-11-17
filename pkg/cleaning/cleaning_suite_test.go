package cleaning_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCleaning(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cleaning Suite")
}
