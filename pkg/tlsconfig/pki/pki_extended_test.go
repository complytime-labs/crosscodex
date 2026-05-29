package pki_test

import (
	"crypto/x509"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

var _ = Describe("Extended PKI Options", func() {
	var ca *pki.CertKeyPair

	BeforeEach(func() {
		var err error
		ca, err = pki.GenerateCA()
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("WithOrgUnit", func() {
		It("sets OrganizationalUnit on the generated certificate", func() {
			cert, err := pki.GenerateCert(ca,
				pki.WithOrgUnit("Engineering"),
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(cert.Cert.Subject.OrganizationalUnit).To(ConsistOf("Engineering"))
		})
	})

	Describe("WithEmailAddresses", func() {
		It("sets email SANs on the generated certificate", func() {
			cert, err := pki.GenerateCert(ca,
				pki.WithEmailAddresses("alice@example.com", "bob@example.com"),
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(cert.Cert.EmailAddresses).To(ConsistOf("alice@example.com", "bob@example.com"))
		})
	})

	Describe("WithURIs", func() {
		It("sets URI SANs on the generated certificate", func() {
			u, err := url.Parse("spiffe://cluster.local/ns/default/sa/web")
			Expect(err).NotTo(HaveOccurred())

			cert, err := pki.GenerateCert(ca,
				pki.WithURIs(u),
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(cert.Cert.URIs).To(HaveLen(1))
			Expect(cert.Cert.URIs[0].String()).To(Equal("spiffe://cluster.local/ns/default/sa/web"))
		})
	})

	Describe("combined options", func() {
		It("sets all extended fields together", func() {
			u, err := url.Parse("spiffe://cluster.local/sa/test")
			Expect(err).NotTo(HaveOccurred())

			cert, err := pki.GenerateCert(ca,
				pki.WithOrganization("Acme Corp"),
				pki.WithOrgUnit("Security"),
				pki.WithEmailAddresses("admin@acme.com"),
				pki.WithURIs(u),
				pki.WithExtKeyUsage(x509.ExtKeyUsageClientAuth),
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(cert.Cert.Subject.Organization).To(ConsistOf("Acme Corp"))
			Expect(cert.Cert.Subject.OrganizationalUnit).To(ConsistOf("Security"))
			Expect(cert.Cert.EmailAddresses).To(ConsistOf("admin@acme.com"))
			Expect(cert.Cert.URIs).To(HaveLen(1))
		})
	})
})
