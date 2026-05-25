package version_test

import (
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/version"
)

func TestVersionBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Version BDD Suite")
}

var _ = Describe("Version Info", func() {
	var info version.Info

	BeforeEach(func() {
		info = version.GetInfo()
	})

	Describe("GetInfo default values", func() {
		It("returns 'dev' as the default version", func() {
			Expect(info.Version).To(Equal("dev"))
		})

		It("returns 'unknown' as the default git commit", func() {
			Expect(info.GitCommit).To(Equal("unknown"))
		})

		It("returns 'unknown' as the default build date", func() {
			Expect(info.BuildDate).To(Equal("unknown"))
		})

		It("returns the current Go runtime version", func() {
			Expect(info.GoVersion).To(Equal(runtime.Version()))
		})

		It("returns the current operating system", func() {
			Expect(info.OS).To(Equal(runtime.GOOS))
		})

		It("returns the current architecture", func() {
			Expect(info.Arch).To(Equal(runtime.GOARCH))
		})
	})

	Describe("Runtime fields are populated", func() {
		It("has a non-empty GoVersion", func() {
			Expect(info.GoVersion).NotTo(BeEmpty())
		})

		It("has a non-empty OS", func() {
			Expect(info.OS).NotTo(BeEmpty())
		})

		It("has a non-empty Arch", func() {
			Expect(info.Arch).NotTo(BeEmpty())
		})
	})
})
