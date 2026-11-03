package jobconfig_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestJobConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "JobConfig Suite")
}
