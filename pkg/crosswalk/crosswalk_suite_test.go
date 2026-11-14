package crosswalk_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCrosswalk(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Crosswalk Suite")
}
