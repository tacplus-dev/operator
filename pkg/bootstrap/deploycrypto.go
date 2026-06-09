package bootstrap

import (
	"crypto/rand"
	stdx509 "crypto/x509"
	stdpkix "crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"net/url"
	"time"

	wrapperKey "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/libs/key"
	wrapperX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/libs/x509"
	protoX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/libs/x509"
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
func GenerateCrypto(cfg *protoOperator.Config, coreCount, edgeCount int, fqdn string) error {
	if cfg.Pki == nil {
		cfg.Pki = &protoOperator.PKI{}
	}

	caImmKey, caCert, err := GenerateRootCA(cfg)
	if err != nil {
		return err
	}

	// Cluster intermediate CA (signed by root)
	clusterCAKey, err := wrapperKey.NewEC384Key()
	if err != nil {
		return fmt.Errorf("generating cluster CA key: %w", err)
	}
	clusterCAKey.Created = time.Now().Unix()

	clusterCAImmKey, err := wrapperKey.NewImmutableKey(clusterCAKey)
	if err != nil {
		return fmt.Errorf("creating immutable cluster CA key: %w", err)
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

	clusterCACertDER, err := stdx509.CreateCertificate(rand.Reader, clusterCATemplate, caCert, clusterCAImmKey.PublicKey(), caImmKey.PrivateKey())
	if err != nil {
		return fmt.Errorf("creating cluster CA certificate: %w", err)
	}

	clusterCACertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clusterCACertDER})
	clusterCACertProto, err := wrapperX509.NewX509Cert(clusterCACertPEM)
	if err != nil {
		return fmt.Errorf("wrapping cluster CA cert: %w", err)
	}
	clusterCACertProto.Id = clusterCAKey.Id

	cfg.Pki.ClusterCa = &protoX509.X509CertChain{
		Chain: []*protoX509.X509Cert{clusterCACertProto},
		Key:   clusterCAKey,
	}

	clusterCACert, err := stdx509.ParseCertificate(clusterCACertDER)
	if err != nil {
		return fmt.Errorf("parsing cluster CA certificate: %w", err)
	}

	// SPIFFE URI for kernel — added to the cert's URI SAN so that
	// SPIFFE-aware clients can verify kernel identity via URI rather than DNS.
	// Defaults to "local/dev" if operator.yaml didn't set spiffe_prefix.
	spiffePrefix := cfg.SpiffePrefix
	if spiffePrefix == "" {
		spiffePrefix = "local/dev"
	}
	kernelSpiffeURI := fmt.Sprintf("spiffe://%s/kernel", spiffePrefix)

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

	// Edge TLS certs (split mode only)
	cfg.Pki.EdgeTls = make([]*protoX509.X509CertChain, edgeCount)
	for i := range edgeCount {
		nodeName := fmt.Sprintf("kernel-edge-%d", i)
		cfg.Pki.EdgeTls[i], err = generateSignedCertChain(clusterCAImmKey, clusterCACert, []string{nodeName}, kernelSpiffeURI)
		if err != nil {
			return fmt.Errorf("generating %s TLS cert: %w", nodeName, err)
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
// `spiffe://${trust_domain}/${service}` workload identity rather than DNS.
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
