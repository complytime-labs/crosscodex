package main

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCrosswalkEvalCmd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Crosswalk Eval Command Suite")
}
