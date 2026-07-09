package candidate_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
)

func TestCandidateBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Candidate Registry BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })
