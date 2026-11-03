package pgstore_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPGStore(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ResultCache PGStore Suite")
}
