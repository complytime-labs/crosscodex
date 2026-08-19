//go:build !integration

package main

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCrosscodexdBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Crosscodexd Daemon BDD Suite")
}
