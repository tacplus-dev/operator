package bootstrap

import (
	"crypto/rand"
	stdx509 "crypto/x509"
	stdpkix "crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"time"

	wrapperKey "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/chassis/key"
	wrapperSpiffe "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/chassis/spiffe"
	wrapperX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/chassis/x509"
	protoKey "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/sys/key"
	protoKmaker "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kmaker"
	protoX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/sys/x509"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
)

const (
	caTTL   = 10 * 365 * 24 * time.Hour // 10 years
	certTTL = 365 * 24 * time.Hour      // 1 year
)

// utf8CN creates a pkix.Name with the CommonName encoded as UTF8String.
// Go's default asn1.Marshal uses PrintableString for ASCII-compatible strings,
// but Java's BouncyCastle uses UTF8String. When the kernel (Java) creates
// intermediate CA certs, it encodes the issuer CN as UTF8String. If the CA cert's
// subject uses PrintableString, rustls/webpki's byte-level comparison fails,
// causing UnknownIssuer errors. Forcing UTF8String here ensures DER-level
// consistency across Go and Java cert creation.
func utf8CN(cn string) stdpkix.Name {
	return stdpkix.Name{
		ExtraNames: []stdpkix.AttributeTypeAndValue{{
			Type: asn1.ObjectIdentifier{2, 5, 4, 3},
			Value: asn1.RawValue{
				Tag:   asn1.TagUTF8String,
				Class: asn1.ClassUniversal,
				Bytes: []byte(cn),
			},
		}},
	}
}

// GenerateRootCA generates a self-signed root CA and sets cfg.Pki.RootCa.
// If cfg.Pki.RootCa is already set, reuses the existing key and cert.
// Returns the immutable key and parsed x509 certificate for signing child certs.
func GenerateRootCA(cfg *protoOperator.Config) (*wrapperKey.ImmutableKey, *stdx509.Certificate, error) {
	if cfg.Pki == nil {
		cfg.Pki = &protoOperator.PKI{}
	}

	if cfg.Pki.RootCa != nil && len(cfg.Pki.RootCa.Chain) > 0 && cfg.Pki.RootCa.Key != nil {
		caImmKey, err := wrapperKey.NewImmutableKey(cfg.Pki.RootCa.Key)
		if err != nil {
			return nil, nil, fmt.Errorf("loading existing root CA key: %w", err)
		}
		cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.RootCa)
		if err != nil {
			return nil, nil, fmt.Errorf("loading existing root CA cert: %w", err)
		}
		return caImmKey, cc.Leaf().X509Certificate(), nil
	}

	// Generate new root CA (ECDSA P-384 for broad TLS compatibility)
	caKey, err := wrapperKey.NewEC384Key()
	if err != nil {
		return nil, nil, fmt.Errorf("generating root CA key: %w", err)
	}
	caKey.Created = time.Now().Unix()

	caImmKey, err := wrapperKey.NewImmutableKey(caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("creating immutable root CA key: %w", err)
	}

	now := time.Now()
	caTemplate := &stdx509.Certificate{
		SerialNumber:          wrapperX509.GenerateSerialNumber(),
		Subject:               utf8CN("kernel-ca"),
		Issuer:                utf8CN("kernel-ca"),
		NotBefore:             now,
		NotAfter:              now.Add(caTTL),
		KeyUsage:              stdx509.KeyUsageCertSign | stdx509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := stdx509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caImmKey.PublicKey(), caImmKey.PrivateKey())
	if err != nil {
		return nil, nil, fmt.Errorf("creating root CA certificate: %w", err)
	}

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
	caCertProto, err := wrapperX509.NewX509Cert(caCertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("wrapping root CA cert: %w", err)
	}
	caCertProto.Id = caKey.Id

	cfg.Pki.RootCa = &protoX509.X509CertChain{
		Chain: []*protoX509.X509Cert{caCertProto},
		Key:   caKey,
	}

	caCert, err := stdx509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing root CA certificate: %w", err)
	}

	return caImmKey, caCert, nil
}

// GenerateCrypto generates all PKI material for the cluster and populates cfg.Pki.
// If cfg.Pki.RootCa is nil, a new root CA is generated via GenerateRootCA.
// coreCount is the number of kernel nodes; edgeCount is for split-node mode (0 for collapsed).
// fqdn is an optional extra SAN added to each node TLS cert.
func GenerateCrypto(cfg *protoOperator.Config, coreCount, edgeCount int, fqdn, spiffeTrustDomain string) error {
	if cfg.Pki == nil {
		cfg.Pki = &protoOperator.PKI{}
	}

	caImmKey, caCert, err := GenerateRootCA(cfg)
	if err != nil {
		return err
	}

	clusterCAImmKey, clusterCACert, err := ensureClusterCA(cfg, caImmKey, caCert)
	if err != nil {
		return err
	}
	if err := ensureKmakerSigningCAWithParent(cfg, clusterCAImmKey, clusterCACert); err != nil {
		return err
	}

	kernelSpiffeURI, err := KernelServiceSpiffeURI(spiffeTrustDomain)
	if err != nil {
		return err
	}

	// Per-node TLS certs (one per kernel replica)
	cfg.Pki.NodeTls = make([]*protoX509.X509CertChain, coreCount)
	for i := range coreCount {
		var nodeName string
		if edgeCount == 0 {
			nodeName = fmt.Sprintf("kernel-%d", i)
		} else {
			nodeName = fmt.Sprintf("kernel-core-%d", i)
		}
		noHyphen := fmt.Sprintf("kernel%d", i)
		// Include StatefulSet DNS: <pod>.<headless-service> (e.g. kernel-0.kernel)
		stsName := fmt.Sprintf("%s.kernel", nodeName)
		// "kernel" = the headless service / service name. The gateway's backend
		// resolver verifies TLS with domain_name = service_id ("kernel"), exactly
		// like every other service, so the cert must carry the bare service name
		// or the gateway can't proxy /kernel.* RPCs.
		dnsNames := []string{nodeName, noHyphen, stsName, "kernel"}
		if fqdn != "" && fqdn != nodeName && fqdn != noHyphen {
			dnsNames = append(dnsNames, fqdn)
		}
		cfg.Pki.NodeTls[i], err = generateSignedCertChain(clusterCAImmKey, clusterCACert, dnsNames, kernelSpiffeURI)
		if err != nil {
			return fmt.Errorf("generating %s TLS cert: %w", nodeName, err)
		}
	}

	// Edge TLS certs (split mode only). Preserve existing EdgeTls entries
	// (e.g. gateway external TLS at index 0) when edgeCount is 0.
	if edgeCount > 0 {
		cfg.Pki.EdgeTls = make([]*protoX509.X509CertChain, edgeCount)
		for i := range edgeCount {
			nodeName := fmt.Sprintf("kernel-edge-%d", i)
			cfg.Pki.EdgeTls[i], err = generateSignedCertChain(clusterCAImmKey, clusterCACert, []string{nodeName}, kernelSpiffeURI)
			if err != nil {
				return fmt.Errorf("generating %s TLS cert: %w", nodeName, err)
			}
		}
	}

	// Per-node signing CAs (for X509Mint token signing)
	cfg.Pki.NodeSigningCa = make([]*protoX509.X509CertChain, coreCount)
	for i := range coreCount {
		var nodeName string
		if edgeCount == 0 {
			nodeName = fmt.Sprintf("kernel-%d", i)
		} else {
			nodeName = fmt.Sprintf("kernel-core-%d", i)
		}
		cfg.Pki.NodeSigningCa[i], err = generateSigningCACertChain(clusterCAImmKey, clusterCACert, nodeName)
		if err != nil {
			return fmt.Errorf("generating %s signing CA: %w", nodeName, err)
		}
	}

	// Edge signing CAs (split mode only)
	cfg.Pki.EdgeSigningCa = make([]*protoX509.X509CertChain, edgeCount)
	for i := range edgeCount {
		nodeName := fmt.Sprintf("kernel-edge-%d", i)
		cfg.Pki.EdgeSigningCa[i], err = generateSigningCACertChain(clusterCAImmKey, clusterCACert, nodeName)
		if err != nil {
			return fmt.Errorf("generating %s signing CA: %w", nodeName, err)
		}
	}

	return nil
}

func ensureClusterCA(cfg *protoOperator.Config, rootKey *wrapperKey.ImmutableKey, rootCert *stdx509.Certificate) (*wrapperKey.ImmutableKey, *stdx509.Certificate, error) {
	if cfg.Pki.ClusterCa != nil && len(cfg.Pki.ClusterCa.Chain) > 0 && cfg.Pki.ClusterCa.Key != nil {
		clusterCAImmKey, err := wrapperKey.NewImmutableKey(cfg.Pki.ClusterCa.Key)
		if err != nil {
			return nil, nil, fmt.Errorf("loading existing cluster CA key: %w", err)
		}
		cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.ClusterCa)
		if err != nil {
			return nil, nil, fmt.Errorf("loading existing cluster CA cert: %w", err)
		}
		return clusterCAImmKey, cc.Leaf().X509Certificate(), nil
	}

	// Cluster intermediate CA (signed by root)
	clusterCAKey, err := wrapperKey.NewEC384Key()
	if err != nil {
		return nil, nil, fmt.Errorf("generating cluster CA key: %w", err)
	}
	clusterCAKey.Created = time.Now().Unix()

	clusterCAImmKey, err := wrapperKey.NewImmutableKey(clusterCAKey)
	if err != nil {
		return nil, nil, fmt.Errorf("creating immutable cluster CA key: %w", err)
	}

	now := time.Now()
	clusterCATemplate := &stdx509.Certificate{
		SerialNumber:          wrapperX509.GenerateSerialNumber(),
		Subject:               utf8CN("cluster-ca"),
		NotBefore:             now,
		NotAfter:              now.Add(caTTL),
		KeyUsage:              stdx509.KeyUsageCertSign | stdx509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	clusterCACertDER, err := stdx509.CreateCertificate(rand.Reader, clusterCATemplate, rootCert, clusterCAImmKey.PublicKey(), rootKey.PrivateKey())
	if err != nil {
		return nil, nil, fmt.Errorf("creating cluster CA certificate: %w", err)
	}

	clusterCACertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clusterCACertDER})
	clusterCACertProto, err := wrapperX509.NewX509Cert(clusterCACertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("wrapping cluster CA cert: %w", err)
	}
	clusterCACertProto.Id = clusterCAKey.Id

	cfg.Pki.ClusterCa = &protoX509.X509CertChain{
		Chain: []*protoX509.X509Cert{clusterCACertProto},
		Key:   clusterCAKey,
	}

	clusterCACert, err := stdx509.ParseCertificate(clusterCACertDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing cluster CA certificate: %w", err)
	}

	return clusterCAImmKey, clusterCACert, nil
}

func kmakerSigningCANeedsGeneration(cfg *protoOperator.Config) bool {
	return cfg == nil ||
		cfg.Pki == nil ||
		cfg.Pki.KmakerSigningCa == nil ||
		len(cfg.Pki.KmakerSigningCa.Chain) == 0 ||
		cfg.Pki.KmakerSigningCa.Key == nil
}

func ensureKmakerSigningCA(cfg *protoOperator.Config) error {
	if !kmakerSigningCANeedsGeneration(cfg) {
		return nil
	}
	if cfg == nil || cfg.Pki == nil || cfg.Pki.ClusterCa == nil || len(cfg.Pki.ClusterCa.Chain) == 0 || cfg.Pki.ClusterCa.Key == nil {
		return fmt.Errorf("pki.cluster_ca with private key is required")
	}
	clusterCAImmKey, err := wrapperKey.NewImmutableKey(cfg.Pki.ClusterCa.Key)
	if err != nil {
		return fmt.Errorf("loading cluster CA key: %w", err)
	}
	cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.ClusterCa)
	if err != nil {
		return fmt.Errorf("loading cluster CA cert: %w", err)
	}
	return ensureKmakerSigningCAWithParent(cfg, clusterCAImmKey, cc.Leaf().X509Certificate())
}

func ensureKmakerSigningCAWithParent(cfg *protoOperator.Config, clusterKey *wrapperKey.ImmutableKey, clusterCert *stdx509.Certificate) error {
	if cfg == nil {
		return fmt.Errorf("operator config is nil")
	}
	if cfg.Pki == nil {
		cfg.Pki = &protoOperator.PKI{}
	}
	if !kmakerSigningCANeedsGeneration(cfg) {
		return nil
	}
	chain, err := generateKmakerSigningCACertChain(clusterKey, clusterCert)
	if err != nil {
		return err
	}
	cfg.Pki.KmakerSigningCa = chain
	return nil
}

// GenerateKernelTLS regenerates only kernel node/edge TLS leaf certificates
// under the existing cluster CA. It preserves the cluster CA and signing CAs so
// trust anchors remain stable during canonical SPIFFE SAN migrations.
func GenerateKernelTLS(cfg *protoOperator.Config, coreCount, edgeCount int, fqdn, spiffeTrustDomain string) error {
	if cfg.Pki == nil || cfg.Pki.ClusterCa == nil || len(cfg.Pki.ClusterCa.Chain) == 0 || cfg.Pki.ClusterCa.Key == nil {
		return fmt.Errorf("pki.cluster_ca with private key is required")
	}
	clusterCAImmKey, err := wrapperKey.NewImmutableKey(cfg.Pki.ClusterCa.Key)
	if err != nil {
		return fmt.Errorf("loading cluster CA key: %w", err)
	}
	cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.ClusterCa)
	if err != nil {
		return fmt.Errorf("loading cluster CA cert: %w", err)
	}
	clusterCACert := cc.Leaf().X509Certificate()

	kernelSpiffeURI, err := KernelServiceSpiffeURI(spiffeTrustDomain)
	if err != nil {
		return err
	}

	cfg.Pki.NodeTls = make([]*protoX509.X509CertChain, coreCount)
	for i := range coreCount {
		var nodeName string
		if edgeCount == 0 {
			nodeName = fmt.Sprintf("kernel-%d", i)
		} else {
			nodeName = fmt.Sprintf("kernel-core-%d", i)
		}
		noHyphen := fmt.Sprintf("kernel%d", i)
		stsName := fmt.Sprintf("%s.kernel", nodeName)
		dnsNames := []string{nodeName, noHyphen, stsName, "kernel"}
		if fqdn != "" && fqdn != nodeName && fqdn != noHyphen {
			dnsNames = append(dnsNames, fqdn)
		}
		cfg.Pki.NodeTls[i], err = generateSignedCertChain(clusterCAImmKey, clusterCACert, dnsNames, kernelSpiffeURI)
		if err != nil {
			return fmt.Errorf("generating %s TLS cert: %w", nodeName, err)
		}
	}

	// Preserve existing EdgeTls entries (e.g. gateway external TLS) when edgeCount is 0.
	if edgeCount > 0 {
		cfg.Pki.EdgeTls = make([]*protoX509.X509CertChain, edgeCount)
		for i := range edgeCount {
			nodeName := fmt.Sprintf("kernel-edge-%d", i)
			cfg.Pki.EdgeTls[i], err = generateSignedCertChain(clusterCAImmKey, clusterCACert, []string{nodeName}, kernelSpiffeURI)
			if err != nil {
				return fmt.Errorf("generating %s TLS cert: %w", nodeName, err)
			}
		}
	}

	return nil
}

// KernelServiceSpiffeURI returns the canonical, location-agnostic kernel
// service identity used in kernel node/edge TLS cert URI SANs.
func KernelServiceSpiffeURI(spiffeTrustDomain string) (string, error) {
	trustDomain := strings.TrimSpace(spiffeTrustDomain)
	if trustDomain == "" {
		trustDomain = "local.dev"
	}
	uri, err := wrapperSpiffe.ServiceSpiffeID(trustDomain, "", "kernel")
	if err != nil {
		return "", fmt.Errorf("building kernel SPIFFE URI: %w", err)
	}
	return uri, nil
}

// KernelTLSNeedsRegeneration reports whether stored kernel node/edge certs are
// absent or lack the canonical kernel service URI SAN.
func KernelTLSNeedsRegeneration(cfg *protoOperator.Config, coreCount, edgeCount int, kernelSpiffeURI string) bool {
	if cfg.Pki == nil {
		return true
	}
	if certChainsNeedSpiffeURI(cfg.Pki.NodeTls, coreCount, kernelSpiffeURI) {
		return true
	}
	return certChainsNeedSpiffeURI(cfg.Pki.EdgeTls, edgeCount, kernelSpiffeURI)
}

func certChainsNeedSpiffeURI(chains []*protoX509.X509CertChain, wantCount int, kernelSpiffeURI string) bool {
	if wantCount == 0 {
		return false
	}
	if len(chains) < wantCount {
		return true
	}
	for i := range wantCount {
		if !certChainHasURI(chains[i], kernelSpiffeURI) {
			return true
		}
	}
	return false
}

func certChainHasURI(chain *protoX509.X509CertChain, uri string) bool {
	if chain == nil || len(chain.Chain) == 0 {
		return false
	}
	cc, err := wrapperX509.NewImmutableX509CertChain(chain)
	if err != nil {
		return false
	}
	for _, san := range cc.Leaf().X509Certificate().URIs {
		if san.String() == uri {
			return true
		}
	}
	return false
}

// RootCACertPEM extracts the PEM-encoded root CA certificate from cfg.Pki.RootCa.
func RootCACertPEM(cfg *protoOperator.Config) ([]byte, error) {
	if cfg.Pki == nil || cfg.Pki.RootCa == nil || len(cfg.Pki.RootCa.Chain) == 0 {
		return nil, fmt.Errorf("no root CA in config")
	}
	cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.RootCa)
	if err != nil {
		return nil, fmt.Errorf("loading root CA cert chain: %w", err)
	}
	return cc.Leaf().Bytes(), nil
}

// generateSignedCertChain creates a leaf TLS cert signed by the cluster CA.
// `dnsNames` populates the DNS SAN list. `spiffeURI`, if non-empty, is added
// as a URI SAN — this lets Rust SPIFFE verifiers identify the peer by its
// canonical service identity rather than DNS.
func generateSignedCertChain(caKey *wrapperKey.ImmutableKey, caCert *stdx509.Certificate, dnsNames []string, spiffeURI string) (*protoX509.X509CertChain, error) {
	serverKey, err := wrapperKey.NewEC384Key()
	if err != nil {
		return nil, fmt.Errorf("generating server key: %w", err)
	}
	serverKey.Created = time.Now().Unix()

	serverImmKey, err := wrapperKey.NewImmutableKey(serverKey)
	if err != nil {
		return nil, fmt.Errorf("creating immutable server key: %w", err)
	}

	now := time.Now()
	template := &stdx509.Certificate{
		SerialNumber:          wrapperX509.GenerateSerialNumber(),
		Subject:               utf8CN(dnsNames[0]),
		NotBefore:             now,
		NotAfter:              now.Add(certTTL),
		KeyUsage:              stdx509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []stdx509.ExtKeyUsage{stdx509.ExtKeyUsageServerAuth, stdx509.ExtKeyUsageClientAuth},
		DNSNames:              dnsNames,
		BasicConstraintsValid: true,
	}
	if spiffeURI != "" {
		u, err := url.Parse(spiffeURI)
		if err != nil {
			return nil, fmt.Errorf("parsing spiffe URI %q: %w", spiffeURI, err)
		}
		template.URIs = []*url.URL{u}
	}

	certDER, err := stdx509.CreateCertificate(rand.Reader, template, caCert, serverImmKey.PublicKey(), caKey.PrivateKey())
	if err != nil {
		return nil, fmt.Errorf("creating certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certProto, err := wrapperX509.NewX509Cert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("wrapping cert: %w", err)
	}
	certProto.Id = serverKey.Id

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	caCertProto, err := wrapperX509.NewX509Cert(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("wrapping CA cert for chain: %w", err)
	}

	return &protoX509.X509CertChain{
		Chain: []*protoX509.X509Cert{certProto, caCertProto},
		Key:   serverKey,
	}, nil
}

func generateSigningCACertChain(caKey *wrapperKey.ImmutableKey, caCert *stdx509.Certificate, nodeName string) (*protoX509.X509CertChain, error) {
	signingKey, err := wrapperKey.NewEC384Key()
	if err != nil {
		return nil, fmt.Errorf("generating signing CA key: %w", err)
	}
	signingKey.Created = time.Now().Unix()

	signingImmKey, err := wrapperKey.NewImmutableKey(signingKey)
	if err != nil {
		return nil, fmt.Errorf("creating immutable signing CA key: %w", err)
	}

	now := time.Now()
	template := &stdx509.Certificate{
		SerialNumber:          wrapperX509.GenerateSerialNumber(),
		Subject:               utf8CN(nodeName + "-signing-ca"),
		NotBefore:             now,
		NotAfter:              now.Add(caTTL),
		KeyUsage:              stdx509.KeyUsageCertSign | stdx509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	certDER, err := stdx509.CreateCertificate(rand.Reader, template, caCert, signingImmKey.PublicKey(), caKey.PrivateKey())
	if err != nil {
		return nil, fmt.Errorf("creating signing CA certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certProto, err := wrapperX509.NewX509Cert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("wrapping signing CA cert: %w", err)
	}
	certProto.Id = signingKey.Id

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	caCertProto, err := wrapperX509.NewX509Cert(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("wrapping CA cert for signing chain: %w", err)
	}

	return &protoX509.X509CertChain{
		Chain: []*protoX509.X509Cert{certProto, caCertProto},
		Key:   signingKey,
	}, nil
}

func generateKmakerSigningCACertChain(caKey *wrapperKey.ImmutableKey, caCert *stdx509.Certificate) (*protoX509.X509CertChain, error) {
	signingKey, err := wrapperKey.NewEC384Key()
	if err != nil {
		return nil, fmt.Errorf("generating kmaker signing CA key: %w", err)
	}
	signingKey.Created = time.Now().Unix()

	signingImmKey, err := wrapperKey.NewImmutableKey(signingKey)
	if err != nil {
		return nil, fmt.Errorf("creating immutable kmaker signing CA key: %w", err)
	}

	now := time.Now()
	notAfter := now.Add(caTTL)
	if caCert.NotAfter.Before(notAfter) {
		notAfter = caCert.NotAfter
	}
	if !notAfter.After(now) {
		return nil, fmt.Errorf("cluster CA is expired")
	}
	template := &stdx509.Certificate{
		SerialNumber:          wrapperX509.GenerateSerialNumber(),
		Subject:               utf8CN("kmaker-kernel-signing-ca"),
		NotBefore:             now,
		NotAfter:              notAfter,
		KeyUsage:              stdx509.KeyUsageCertSign | stdx509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	certDER, err := stdx509.CreateCertificate(rand.Reader, template, caCert, signingImmKey.PublicKey(), caKey.PrivateKey())
	if err != nil {
		return nil, fmt.Errorf("creating kmaker signing CA certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certProto, err := wrapperX509.NewX509Cert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("wrapping kmaker signing CA cert: %w", err)
	}
	certProto.Id = signingKey.Id

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	caCertProto, err := wrapperX509.NewX509Cert(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("wrapping CA cert for kmaker signing chain: %w", err)
	}

	return &protoX509.X509CertChain{
		Chain: []*protoX509.X509Cert{certProto, caCertProto},
		Key:   signingKey,
	}, nil
}

// EnsureGatewayExternalTLS generates a gateway external TLS certificate signed
// by the cluster CA if one does not already exist. The cert is stored in
// cfg.Pki.EdgeTls[0] (the proto field designated for gateway/edge TLS).
// The certificate's DNS SANs include fqdn and localhost for local access.
// Returns true if a new cert was generated.
func EnsureGatewayExternalTLS(cfg *protoOperator.Config, fqdn string) (bool, error) {
	if cfg.Pki == nil || cfg.Pki.ClusterCa == nil || len(cfg.Pki.ClusterCa.Chain) == 0 || cfg.Pki.ClusterCa.Key == nil {
		return false, fmt.Errorf("pki.cluster_ca with private key is required for gateway external TLS")
	}

	// Idempotent: skip if a gateway external TLS cert already exists.
	if len(cfg.Pki.EdgeTls) > 0 && cfg.Pki.EdgeTls[0] != nil &&
		len(cfg.Pki.EdgeTls[0].Chain) > 0 && cfg.Pki.EdgeTls[0].Key != nil {
		return false, nil
	}

	clusterCAImmKey, err := wrapperKey.NewImmutableKey(cfg.Pki.ClusterCa.Key)
	if err != nil {
		return false, fmt.Errorf("loading cluster CA key: %w", err)
	}
	cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.ClusterCa)
	if err != nil {
		return false, fmt.Errorf("loading cluster CA cert: %w", err)
	}
	clusterCACert := cc.Leaf().X509Certificate()

	dnsNames := []string{fqdn}
	if fqdn != "localhost" {
		dnsNames = append(dnsNames, "localhost")
	}

	chain, err := generateSignedCertChain(clusterCAImmKey, clusterCACert, dnsNames, "")
	if err != nil {
		return false, fmt.Errorf("generating gateway external TLS cert: %w", err)
	}

	if len(cfg.Pki.EdgeTls) == 0 {
		cfg.Pki.EdgeTls = []*protoX509.X509CertChain{chain}
	} else {
		cfg.Pki.EdgeTls[0] = chain
	}

	return true, nil
}

// EnsureKmakerServingTLS generates the serving certificate for kmaker's own
// gRPC listener, signed by the cluster CA, if one does not already exist. The
// cert is stored in cfg.Kmaker.Tls (kmaker.Config field 10), which
// BuildKmakerConfig copies into the generated kmaker service config.
// Returns true if a new cert was generated.
//
// Unlike the kernel/gateway certs this material is not part of cfg.Pki, so it
// is not persisted to crypto.yaml — it is regenerated whenever the operator
// process reloads its config, and reused for the lifetime of that cfg.
func EnsureKmakerServingTLS(cfg *protoOperator.Config) (bool, error) {
	if cfg.Pki == nil || cfg.Pki.ClusterCa == nil || len(cfg.Pki.ClusterCa.Chain) == 0 || cfg.Pki.ClusterCa.Key == nil {
		return false, fmt.Errorf("pki.cluster_ca with private key is required for kmaker serving TLS")
	}

	// Idempotent: skip if a kmaker serving cert already exists.
	if cfg.Kmaker != nil && cfg.Kmaker.Tls != nil &&
		len(cfg.Kmaker.Tls.Chain) > 0 && cfg.Kmaker.Tls.Key != nil {
		return false, nil
	}

	clusterCAImmKey, err := wrapperKey.NewImmutableKey(cfg.Pki.ClusterCa.Key)
	if err != nil {
		return false, fmt.Errorf("loading cluster CA key: %w", err)
	}
	cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.ClusterCa)
	if err != nil {
		return false, fmt.Errorf("loading cluster CA cert: %w", err)
	}
	clusterCACert := cc.Leaf().X509Certificate()

	chain, err := generateSignedCertChain(clusterCAImmKey, clusterCACert, kmakerServingDNSNames(cfg), "")
	if err != nil {
		return false, fmt.Errorf("generating kmaker serving TLS cert: %w", err)
	}

	if cfg.Kmaker == nil {
		cfg.Kmaker = &protoKmaker.Config{}
	}
	cfg.Kmaker.Tls = chain

	return true, nil
}

// kmakerServingDNSNames lists the names clients may use to reach kmaker.
// Same shape as the kernel node certs (own name + service DNS): "kmaker" is
// the service id a SPIFFE/gateway-style verifier uses as domain_name, and
// "kmaker0" is the Deployment/Service name plus its in-cluster DNS forms.
func kmakerServingDNSNames(cfg *protoOperator.Config) []string {
	ns := cfg.Namespace
	if ns == "" {
		ns = "schematic"
	}
	dnsNames := []string{
		"kmaker",
		"kmaker0",
		fmt.Sprintf("kmaker0.%s.svc", ns),
		fmt.Sprintf("kmaker0.%s.svc.cluster.local", ns),
	}
	if cfg.Fqdn != "" {
		dnsNames = append(dnsNames, cfg.Fqdn)
	}
	return dnsNames
}

// GatewayExternalTLSPEM extracts PEM-encoded certificate chain and private key
// from the gateway external TLS cert stored in cfg.Pki.EdgeTls[0].
func GatewayExternalTLSPEM(cfg *protoOperator.Config) (certPEM string, keyPEM string, err error) {
	if cfg.Pki == nil || len(cfg.Pki.EdgeTls) == 0 || cfg.Pki.EdgeTls[0] == nil {
		return "", "", fmt.Errorf("no gateway external TLS cert in PKI")
	}

	chain := cfg.Pki.EdgeTls[0]
	cc, err := wrapperX509.NewImmutableX509CertChain(chain)
	if err != nil {
		return "", "", fmt.Errorf("loading gateway external TLS cert chain: %w", err)
	}

	// Concatenate all cert PEMs in the chain.
	var certBuf strings.Builder
	for _, cert := range cc.Chain() {
		certBuf.Write(cert.Bytes())
	}

	// The proto Key's Base64 field holds base64-encoded PEM bytes.
	keyBytes, err := decodeKeyPEM(chain.Key)
	if err != nil {
		return "", "", fmt.Errorf("decoding gateway external TLS key: %w", err)
	}

	return certBuf.String(), string(keyBytes), nil
}

// decodeKeyPEM decodes the PEM-encoded private key from a proto Key's Base64 field.
func decodeKeyPEM(k *protoKey.Key) ([]byte, error) {
	if k == nil || k.Base64 == "" {
		return nil, fmt.Errorf("key is nil or has no base64 data")
	}
	decoders := []func(string) ([]byte, error){
		func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) },
		func(s string) ([]byte, error) { return base64.RawStdEncoding.DecodeString(s) },
		func(s string) ([]byte, error) { return base64.URLEncoding.DecodeString(s) },
		func(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) },
	}
	for _, dec := range decoders {
		if b, err := dec(k.Base64); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("cannot decode key base64")
}

// blobstoreTokenKeyBytes is the length of blobstore's upload-token signing key.
// 32 bytes is HMAC-SHA256's block-optimal key length and is the minimum the
// service accepts (NewTokenSigner refuses anything shorter).
const blobstoreTokenKeyBytes = 32

// EnsureBlobstoreTokenKey generates the upload-token signing key if the cluster
// has none, and leaves an existing one alone.
//
// IDEMPOTENT ON PURPOSE, and that is the whole contract: both blobstore pods must
// verify the same signature, and a key regenerated on a later `deploy` would
// invalidate every upload token a client is holding at that moment. So this is
// generate-if-absent, never rotate. Rotating it is a deliberate act — delete the
// field and redeploy, accepting that in-flight upload tokens stop working — and is
// not something a routine deploy should be able to do by accident.
func EnsureBlobstoreTokenKey(cfg *protoOperator.Config) error {
	if cfg.GetPki().GetBlobstoreTokenKey() != "" {
		return nil
	}
	key := make([]byte, blobstoreTokenKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generating blobstore token key: %w", err)
	}
	if cfg.Pki == nil {
		cfg.Pki = &protoOperator.PKI{}
	}
	// Standard base64: the service accepts either alphabet, and this is the one
	// every other base64 value in this config uses.
	cfg.Pki.BlobstoreTokenKey = base64.StdEncoding.EncodeToString(key)
	return nil
}
