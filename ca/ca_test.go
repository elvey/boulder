package ca

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"fmt"
	"io/ioutil"
	"sort"
	"testing"
	"time"

	cfsslConfig "github.com/cloudflare/cfssl/config"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/golang/mock/gomock"
	"github.com/jmhodges/clock"
	"github.com/letsencrypt/boulder/metrics/mock_metrics"
	"golang.org/x/crypto/ocsp"
	"golang.org/x/net/context"

	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/core"
	berrors "github.com/letsencrypt/boulder/errors"
	"github.com/letsencrypt/boulder/goodkey"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics"
	"github.com/letsencrypt/boulder/mocks"
	"github.com/letsencrypt/boulder/policy"
	"github.com/letsencrypt/boulder/test"
)

var (
	// * Random public key
	// * CN = not-example.com
	// * DNSNames = not-example.com, www.not-example.com
	CNandSANCSR = mustRead("./testdata/cn_and_san.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * C = US
	// * CN = [none]
	// * DNSNames = not-example.com
	NoCNCSR = mustRead("./testdata/no_cn.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * C = US
	// * CN = [none]
	// * DNSNames = [none]
	NoNameCSR = mustRead("./testdata/no_name.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = [none]
	// * DNSNames = not-example.com, www.not-example.com, mail.example.com
	TooManyNameCSR = mustRead("./testdata/too_many_names.der.csr")

	// CSR generated by Go:
	// * Random public key -- 512 bits long
	// * CN = (none)
	// * DNSNames = not-example.com, www.not-example.com, mail.not-example.com
	ShortKeyCSR = mustRead("./testdata/short_key.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = CapiTalizedLetters.com
	// * DNSNames = moreCAPs.com, morecaps.com, evenMOREcaps.com, Capitalizedletters.COM
	CapitalizedCSR = mustRead("./testdata/capitalized_cn_and_san.der.csr")

	// CSR generated by OpenSSL:
	// Edited signature to become invalid.
	WrongSignatureCSR = mustRead("./testdata/invalid_signature.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = not-example.com
	// * Includes an extensionRequest attribute for a well-formed TLS Feature extension
	MustStapleCSR = mustRead("./testdata/must_staple.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = not-example.com
	// * Includes extensionRequest attributes for *two* must-staple extensions
	DuplicateMustStapleCSR = mustRead("./testdata/duplicate_must_staple.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = not-example.com
	// * Includes an extensionRequest attribute for an empty TLS Feature extension
	TLSFeatureUnknownCSR = mustRead("./testdata/tls_feature_unknown.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = not-example.com
	// * Includes an extensionRequest attribute for the CT Poison extension (not supported)
	UnsupportedExtensionCSR = mustRead("./testdata/unsupported_extension.der.csr")

	// CSR generated by Go:
	// * Random ECDSA public key.
	// * CN = [none]
	// * DNSNames = example.com, example2.com
	ECDSACSR = mustRead("./testdata/ecdsa.der.csr")

	// CSR generated by Go:
	// * Random RSA public key.
	// * CN = [none]
	// * DNSNames = [none]
	NoNamesCSR = mustRead("./testdata/no_names.der.csr")

	// CSR generated by Go:
	// * Random RSA public key.
	// * CN = aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com
	// * DNSNames = [none]
	LongCNCSR = mustRead("./testdata/long_cn.der.csr")

	log = blog.UseMock()
)

// CFSSL config
const rsaProfileName = "rsaEE"
const ecdsaProfileName = "ecdsaEE"
const caKeyFile = "../test/test-ca.key"
const caCertFile = "../test/test-ca.pem"

func mustRead(path string) []byte {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("unable to read %#v: %s", path, err))
	}
	return b
}

type testCtx struct {
	caConfig  cmd.CAConfig
	pa        core.PolicyAuthority
	issuers   []Issuer
	keyPolicy goodkey.KeyPolicy
	fc        clock.FakeClock
	stats     metrics.Scope
	logger    blog.Logger
}

type mockSA struct {
	certificate core.Certificate
}

func (m *mockSA) AddCertificate(ctx context.Context, der []byte, _ int64, _ []byte) (string, error) {
	m.certificate.DER = der
	return "", nil
}

var caKey crypto.Signer
var caCert *x509.Certificate
var ctx = context.Background()

func init() {
	var err error
	caKey, err = helpers.ParsePrivateKeyPEM(mustRead(caKeyFile))
	if err != nil {
		panic(fmt.Sprintf("Unable to parse %s: %s", caKeyFile, err))
	}
	caCert, err = core.LoadCert(caCertFile)
	if err != nil {
		panic(fmt.Sprintf("Unable to parse %s: %s", caCertFile, err))
	}
}

func setup(t *testing.T) *testCtx {
	fc := clock.NewFake()
	fc.Add(1 * time.Hour)

	pa, err := policy.New(nil)
	test.AssertNotError(t, err, "Couldn't create PA")
	err = pa.SetHostnamePolicyFile("../test/hostname-policy.json")
	test.AssertNotError(t, err, "Couldn't set hostname policy")

	// Create a CA
	caConfig := cmd.CAConfig{
		RSAProfile:   rsaProfileName,
		ECDSAProfile: ecdsaProfileName,
		SerialPrefix: 17,
		Expiry:       "8760h",
		LifespanOCSP: cmd.ConfigDuration{Duration: 45 * time.Minute},
		MaxNames:     2,
		CFSSL: cfsslConfig.Config{
			Signing: &cfsslConfig.Signing{
				Profiles: map[string]*cfsslConfig.SigningProfile{
					rsaProfileName: {
						Usage:     []string{"digital signature", "key encipherment", "server auth"},
						IssuerURL: []string{"http://not-example.com/issuer-url"},
						OCSP:      "http://not-example.com/ocsp",
						CRL:       "http://not-example.com/crl",

						Policies: []cfsslConfig.CertificatePolicy{
							{
								ID: cfsslConfig.OID(asn1.ObjectIdentifier{2, 23, 140, 1, 2, 1}),
							},
						},
						ExpiryString: "8760h",
						Backdate:     time.Hour,
						CSRWhitelist: &cfsslConfig.CSRWhitelist{
							PublicKeyAlgorithm: true,
							PublicKey:          true,
							SignatureAlgorithm: true,
						},
						ClientProvidesSerialNumbers: true,
						AllowedExtensions: []cfsslConfig.OID{
							cfsslConfig.OID(oidTLSFeature),
						},
					},
					ecdsaProfileName: {
						Usage:     []string{"digital signature", "server auth"},
						IssuerURL: []string{"http://not-example.com/issuer-url"},
						OCSP:      "http://not-example.com/ocsp",
						CRL:       "http://not-example.com/crl",

						Policies: []cfsslConfig.CertificatePolicy{
							{
								ID: cfsslConfig.OID(asn1.ObjectIdentifier{2, 23, 140, 1, 2, 1}),
							},
						},
						ExpiryString: "8760h",
						Backdate:     time.Hour,
						CSRWhitelist: &cfsslConfig.CSRWhitelist{
							PublicKeyAlgorithm: true,
							PublicKey:          true,
							SignatureAlgorithm: true,
						},
						ClientProvidesSerialNumbers: true,
					},
				},
				Default: &cfsslConfig.SigningProfile{
					ExpiryString: "8760h",
				},
			},
		},
	}

	issuers := []Issuer{{caKey, caCert}}

	keyPolicy := goodkey.KeyPolicy{
		AllowRSA:           true,
		AllowECDSANISTP256: true,
		AllowECDSANISTP384: true,
	}

	logger := blog.NewMock()

	return &testCtx{
		caConfig,
		pa,
		issuers,
		keyPolicy,
		fc,
		metrics.NewNoopScope(),
		logger,
	}
}

func TestFailNoSerial(t *testing.T) {
	testCtx := setup(t)

	testCtx.caConfig.SerialPrefix = 0
	_, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	test.AssertError(t, err, "CA should have failed with no SerialPrefix")
}

func TestIssueCertificate(t *testing.T) {
	testCtx := setup(t)
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.forceCNFromSAN = false
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	sa := &mockSA{}
	ca.SA = sa

	csr, _ := x509.ParseCertificateRequest(CNandSANCSR)

	// Sign CSR
	issuedCert, err := ca.IssueCertificate(ctx, *csr, 1001)
	test.AssertNotError(t, err, "Failed to sign certificate")

	// Verify cert contents
	cert, err := x509.ParseCertificate(issuedCert.DER)
	test.AssertNotError(t, err, "Certificate failed to parse")

	test.AssertEquals(t, cert.Subject.CommonName, "not-example.com")

	if len(cert.DNSNames) == 1 {
		if cert.DNSNames[0] != "not-example.com" {
			t.Errorf("Improper list of domain names %v", cert.DNSNames)
		} else {
		}
		t.Errorf("Improper list of domain names %v", cert.DNSNames)
	}

	if len(cert.Subject.Country) > 0 {
		t.Errorf("Subject contained unauthorized values: %v", cert.Subject)
	}

	// Verify that the cert got stored in the DB
	serialString := core.SerialToString(cert.SerialNumber)
	if cert.Subject.SerialNumber != serialString {
		t.Errorf("SerialNumber: want %#v, got %#v", serialString, cert.Subject.SerialNumber)
	}
	test.Assert(t, bytes.Equal(issuedCert.DER, sa.certificate.DER), "Retrieved cert not equal to issued cert.")
}

// Test issuing when multiple issuers are present.
func TestIssueCertificateMultipleIssuers(t *testing.T) {
	testCtx := setup(t)
	// Load multiple issuers, and ensure the first one in the list is used.
	newIssuerCert, err := core.LoadCert("../test/test-ca2.pem")
	test.AssertNotError(t, err, "Failed to load new cert")
	newIssuers := []Issuer{
		{
			Signer: caKey,
			// newIssuerCert is first, so it will be the default.
			Cert: newIssuerCert,
		}, {
			Signer: caKey,
			Cert:   caCert,
		},
	}
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		newIssuers,
		testCtx.keyPolicy,
		testCtx.logger)
	test.AssertNotError(t, err, "Failed to remake CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	csr, _ := x509.ParseCertificateRequest(CNandSANCSR)
	issuedCert, err := ca.IssueCertificate(ctx, *csr, 1001)
	test.AssertNotError(t, err, "Failed to sign certificate")

	cert, err := x509.ParseCertificate(issuedCert.DER)
	test.AssertNotError(t, err, "Certificate failed to parse")
	// Verify cert was signed by newIssuerCert, not caCert.
	err = cert.CheckSignatureFrom(newIssuerCert)
	test.AssertNotError(t, err, "Certificate failed signature validation")
}

func TestOCSP(t *testing.T) {
	testCtx := setup(t)
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	csr, _ := x509.ParseCertificateRequest(CNandSANCSR)
	cert, err := ca.IssueCertificate(ctx, *csr, 1001)
	test.AssertNotError(t, err, "Failed to issue")
	parsedCert, err := x509.ParseCertificate(cert.DER)
	test.AssertNotError(t, err, "Failed to parse cert")
	ocspResp, err := ca.GenerateOCSP(ctx, core.OCSPSigningRequest{
		CertDER: cert.DER,
		Status:  string(core.OCSPStatusGood),
	})
	test.AssertNotError(t, err, "Failed to generate OCSP")
	parsed, err := ocsp.ParseResponse(ocspResp, caCert)
	test.AssertNotError(t, err, "Failed to parse validate OCSP")
	test.AssertEquals(t, parsed.Status, 0)
	test.AssertEquals(t, parsed.RevocationReason, 0)
	test.AssertEquals(t, parsed.SerialNumber.Cmp(parsedCert.SerialNumber), 0)

	// Test that signatures are checked.
	_, err = ca.GenerateOCSP(ctx, core.OCSPSigningRequest{
		CertDER: append(cert.DER, byte(0)),
		Status:  string(core.OCSPStatusGood),
	})
	test.AssertError(t, err, "Generated OCSP for cert with bad signature")

	// Load multiple issuers, including the old issuer, and ensure OCSP is still
	// signed correctly.
	newIssuerCert, err := core.LoadCert("../test/test-ca2.pem")
	test.AssertNotError(t, err, "Failed to load new cert")
	newIssuers := []Issuer{
		{
			Signer: caKey,
			// newIssuerCert is first, so it will be the default.
			Cert: newIssuerCert,
		}, {
			Signer: caKey,
			Cert:   caCert,
		},
	}
	ca, err = NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		newIssuers,
		testCtx.keyPolicy,
		testCtx.logger)
	test.AssertNotError(t, err, "Failed to remake CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	// Now issue a new cert, signed by newIssuerCert
	newCert, err := ca.IssueCertificate(ctx, *csr, 1001)
	test.AssertNotError(t, err, "Failed to issue newCert")
	parsedNewCert, err := x509.ParseCertificate(newCert.DER)
	test.AssertNotError(t, err, "Failed to parse newCert")

	err = parsedNewCert.CheckSignatureFrom(newIssuerCert)
	t.Logf("check sig: %s", err)

	// ocspResp2 is a second OCSP response for `cert` (issued by caCert), and
	// should be signed by caCert.
	ocspResp2, err := ca.GenerateOCSP(ctx, core.OCSPSigningRequest{
		CertDER: append(cert.DER),
		Status:  string(core.OCSPStatusGood),
	})
	test.AssertNotError(t, err, "Failed to sign second OCSP response")
	_, err = ocsp.ParseResponse(ocspResp2, caCert)
	test.AssertNotError(t, err, "Failed to parse / validate second OCSP response")

	// newCertOcspResp is an OCSP response for `newCert` (issued by newIssuer),
	// and should be signed by newIssuer.
	newCertOcspResp, err := ca.GenerateOCSP(ctx, core.OCSPSigningRequest{
		CertDER: newCert.DER,
		Status:  string(core.OCSPStatusGood),
	})
	test.AssertNotError(t, err, "Failed to generate OCSP")
	parsedNewCertOcspResp, err := ocsp.ParseResponse(newCertOcspResp, newIssuerCert)
	test.AssertNotError(t, err, "Failed to parse / validate OCSP for newCert")
	test.AssertEquals(t, parsedNewCertOcspResp.Status, 0)
	test.AssertEquals(t, parsedNewCertOcspResp.RevocationReason, 0)
	test.AssertEquals(t, parsedNewCertOcspResp.SerialNumber.Cmp(parsedNewCert.SerialNumber), 0)
}

func TestNoHostnames(t *testing.T) {
	testCtx := setup(t)
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	csr, _ := x509.ParseCertificateRequest(NoNamesCSR)
	_, err = ca.IssueCertificate(ctx, *csr, 1001)
	test.AssertError(t, err, "Issued certificate with no names")
	test.Assert(t, berrors.Is(err, berrors.Malformed), "Incorrect error type returned")
}

func TestRejectTooManyNames(t *testing.T) {
	testCtx := setup(t)
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	// Test that the CA rejects a CSR with too many names
	csr, _ := x509.ParseCertificateRequest(TooManyNameCSR)
	_, err = ca.IssueCertificate(ctx, *csr, 1001)
	test.AssertError(t, err, "Issued certificate with too many names")
	test.Assert(t, berrors.Is(err, berrors.Malformed), "Incorrect error type returned")
}

func TestRejectValidityTooLong(t *testing.T) {
	testCtx := setup(t)
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	// This time is a few minutes before the notAfter in testdata/ca_cert.pem
	future, err := time.Parse(time.RFC3339, "2025-02-10T00:30:00Z")

	test.AssertNotError(t, err, "Failed to parse time")
	testCtx.fc.Set(future)
	// Test that the CA rejects CSRs that would expire after the intermediate cert
	csr, _ := x509.ParseCertificateRequest(NoCNCSR)
	_, err = ca.IssueCertificate(ctx, *csr, 1)
	test.AssertError(t, err, "Cannot issue a certificate that expires after the intermediate certificate")
	test.Assert(t, berrors.Is(err, berrors.InternalServer), "Incorrect error type returned")
}

func TestShortKey(t *testing.T) {
	testCtx := setup(t)
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	// Test that the CA rejects CSRs that would expire after the intermediate cert
	csr, _ := x509.ParseCertificateRequest(ShortKeyCSR)
	_, err = ca.IssueCertificate(ctx, *csr, 1001)
	test.AssertError(t, err, "Issued a certificate with too short a key.")
	test.Assert(t, berrors.Is(err, berrors.Malformed), "Incorrect error type returned")
}

func TestAllowNoCN(t *testing.T) {
	testCtx := setup(t)
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	test.AssertNotError(t, err, "Couldn't create new CA")
	ca.forceCNFromSAN = false
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	csr, err := x509.ParseCertificateRequest(NoCNCSR)
	test.AssertNotError(t, err, "Couldn't parse CSR")
	issuedCert, err := ca.IssueCertificate(ctx, *csr, 1001)
	test.AssertNotError(t, err, "Failed to sign certificate")
	cert, err := x509.ParseCertificate(issuedCert.DER)
	test.AssertNotError(t, err, fmt.Sprintf("unable to parse no CN cert: %s", err))
	if cert.Subject.CommonName != "" {
		t.Errorf("want no CommonName, got %#v", cert.Subject.CommonName)
	}
	serial := core.SerialToString(cert.SerialNumber)
	if cert.Subject.SerialNumber != serial {
		t.Errorf("SerialNumber: want %#v, got %#v", serial, cert.Subject.SerialNumber)
	}

	expected := []string{}
	for _, name := range csr.DNSNames {
		expected = append(expected, name)
	}
	sort.Strings(expected)
	actual := []string{}
	for _, name := range cert.DNSNames {
		actual = append(actual, name)
	}
	sort.Strings(actual)
	test.AssertDeepEquals(t, actual, expected)
}

func TestLongCommonName(t *testing.T) {
	testCtx := setup(t)
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	csr, _ := x509.ParseCertificateRequest(LongCNCSR)
	_, err = ca.IssueCertificate(ctx, *csr, 1001)
	test.AssertError(t, err, "Issued a certificate with a CN over 64 bytes.")
	test.Assert(t, berrors.Is(err, berrors.Malformed), "Incorrect error type returned")
}

func TestWrongSignature(t *testing.T) {
	testCtx := setup(t)
	testCtx.caConfig.MaxNames = 3
	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	// x509.ParseCertificateRequest() does not check for invalid signatures...
	csr, _ := x509.ParseCertificateRequest(WrongSignatureCSR)

	_, err = ca.IssueCertificate(ctx, *csr, 1001)
	if err == nil {
		t.Fatalf("Issued a certificate based on a CSR with an invalid signature.")
	}
}

func TestProfileSelection(t *testing.T) {
	testCtx := setup(t)
	testCtx.caConfig.MaxNames = 3
	ca, _ := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		testCtx.stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	testCases := []struct {
		CSR              []byte
		ExpectedKeyUsage x509.KeyUsage
	}{
		{CNandSANCSR, x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment},
		{ECDSACSR, x509.KeyUsageDigitalSignature},
	}

	for _, testCase := range testCases {
		csr, err := x509.ParseCertificateRequest(testCase.CSR)
		test.AssertNotError(t, err, "Cannot parse CSR")

		// Sign CSR
		issuedCert, err := ca.IssueCertificate(ctx, *csr, 1001)
		test.AssertNotError(t, err, "Failed to sign certificate")

		// Verify cert contents
		cert, err := x509.ParseCertificate(issuedCert.DER)
		test.AssertNotError(t, err, "Certificate failed to parse")

		t.Logf("expected key usage %v, got %v", testCase.ExpectedKeyUsage, cert.KeyUsage)
		test.AssertEquals(t, cert.KeyUsage, testCase.ExpectedKeyUsage)
	}
}

func countMustStaple(t *testing.T, cert *x509.Certificate) (count int) {
	oidTLSFeature := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 24}
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(oidTLSFeature) {
			test.Assert(t, !ext.Critical, "Extension was marked critical")
			test.AssertByteEquals(t, ext.Value, mustStapleFeatureValue)
			count++
		}
	}
	return count
}

func TestExtensions(t *testing.T) {
	testCtx := setup(t)
	testCtx.caConfig.MaxNames = 3

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	stats := mock_metrics.NewMockScope(ctrl)

	ca, err := NewCertificateAuthorityImpl(
		testCtx.caConfig,
		testCtx.fc,
		stats,
		testCtx.issuers,
		testCtx.keyPolicy,
		testCtx.logger)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = testCtx.pa
	ca.SA = &mockSA{}

	mustStapleCSR, err := x509.ParseCertificateRequest(MustStapleCSR)
	test.AssertNotError(t, err, "Error parsing MustStapleCSR")

	duplicateMustStapleCSR, err := x509.ParseCertificateRequest(DuplicateMustStapleCSR)
	test.AssertNotError(t, err, "Error parsing DuplicateMustStapleCSR")

	tlsFeatureUnknownCSR, err := x509.ParseCertificateRequest(TLSFeatureUnknownCSR)
	test.AssertNotError(t, err, "Error parsing TLSFeatureUnknownCSR")

	unsupportedExtensionCSR, err := x509.ParseCertificateRequest(UnsupportedExtensionCSR)
	test.AssertNotError(t, err, "Error parsing UnsupportedExtensionCSR")

	sign := func(csr *x509.CertificateRequest) *x509.Certificate {
		coreCert, err := ca.IssueCertificate(ctx, *csr, 1001)
		test.AssertNotError(t, err, "Failed to issue")
		cert, err := x509.ParseCertificate(coreCert.DER)
		test.AssertNotError(t, err, "Error parsing certificate produced by CA")
		return cert
	}

	// With ca.enableMustStaple = false, should issue successfully and not add
	// Must Staple.
	stats.EXPECT().Inc(metricCSRExtensionTLSFeature, int64(1)).Return(nil)
	stats.EXPECT().Inc("Signatures.Certificate", int64(1)).Return(nil)
	noStapleCert := sign(mustStapleCSR)
	test.AssertEquals(t, countMustStaple(t, noStapleCert), 0)

	// With ca.enableMustStaple = true, a TLS feature extension should put a must-staple
	// extension into the cert
	ca.enableMustStaple = true
	stats.EXPECT().Inc(metricCSRExtensionTLSFeature, int64(1)).Return(nil)
	stats.EXPECT().Inc("Signatures.Certificate", int64(1)).Return(nil)
	singleStapleCert := sign(mustStapleCSR)
	test.AssertEquals(t, countMustStaple(t, singleStapleCert), 1)

	// Even if there are multiple TLS Feature extensions, only one extension should be included
	stats.EXPECT().Inc(metricCSRExtensionTLSFeature, int64(1)).Return(nil)
	stats.EXPECT().Inc("Signatures.Certificate", int64(1)).Return(nil)
	duplicateMustStapleCert := sign(duplicateMustStapleCSR)
	test.AssertEquals(t, countMustStaple(t, duplicateMustStapleCert), 1)

	// ... but if it doesn't ask for stapling, there should be an error
	stats.EXPECT().Inc(metricCSRExtensionTLSFeature, int64(1)).Return(nil)
	stats.EXPECT().Inc(metricCSRExtensionTLSFeatureInvalid, int64(1)).Return(nil)
	_, err = ca.IssueCertificate(ctx, *tlsFeatureUnknownCSR, 1001)
	test.AssertError(t, err, "Allowed a CSR with an empty TLS feature extension")
	test.Assert(t, berrors.Is(err, berrors.Malformed), "Wrong error type when rejecting a CSR with empty TLS feature extension")

	// Unsupported extensions should be silently ignored, having the same
	// extensions as the TLS Feature cert above, minus the TLS Feature Extension
	stats.EXPECT().Inc(metricCSRExtensionOther, int64(1)).Return(nil)
	stats.EXPECT().Inc("Signatures.Certificate", int64(1)).Return(nil)
	unsupportedExtensionCert := sign(unsupportedExtensionCSR)
	test.AssertEquals(t, len(unsupportedExtensionCert.Extensions), len(singleStapleCert.Extensions)-1)
}
