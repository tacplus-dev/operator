package bootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	stdx509 "crypto/x509"
	"encoding/base64"
	"fmt"
	"time"

	wrapperKey "gitlab.com/seabeam/schematic/platform/services/operator/model/chassis/key"
	wrapperX509 "gitlab.com/seabeam/schematic/platform/services/operator/model/chassis/x509"
	protoJwk "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/jwk"
	protoKey "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/key"
	protoX509 "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/x509"
)

const tlsTTL = 1 * time.Hour

// GenerateTLSCertChain creates a self-signed ECDSA P-384 TLS certificate chain.
// Uses P-384 instead of Ed25519 for broad TLS compatibility (some Java/Netty
// stacks do not support Ed25519 TLS certificates).
func GenerateTLSCertChain() (*protoX509.X509CertChain, error) {
	protoKeyMsg, err := wrapperKey.NewEC384Key()
	if err != nil {
		return nil, fmt.Errorf("generating TLS key: %w", err)
	}
	protoKeyMsg.Created = time.Now().Unix()

	immKey, err := wrapperKey.NewImmutableKey(protoKeyMsg)
	if err != nil {
		return nil, fmt.Errorf("creating immutable key: %w", err)
	}

	now := time.Now()
	serial := wrapperX509.GenerateSerialNumber()

	template := &stdx509.Certificate{
		SerialNumber:          serial,
		Subject:               utf8CN("mock-token"),
		Issuer:                utf8CN("mock-token"),
		NotBefore:             now,
		NotAfter:              now.Add(tlsTTL),
		KeyUsage:              stdx509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []stdx509.ExtKeyUsage{stdx509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
	}

	certDER, err := stdx509.CreateCertificate(rand.Reader, template, template, immKey.PublicKey(), immKey.PrivateKey())
	if err != nil {
		return nil, fmt.Errorf("creating certificate: %w", err)
	}

	protoCert, err := wrapperX509.NewX509Cert(certDER)
	if err != nil {
		return nil, fmt.Errorf("wrapping certificate: %w", err)
	}
	protoCert.Id = protoKeyMsg.Id

	return &protoX509.X509CertChain{
		Chain: []*protoX509.X509Cert{protoCert},
		Key:   protoKeyMsg,
	}, nil
}

// GenerateCredPair generates an Ed25519 key pair and returns the public JWK
// proto and the private key proto.
func GenerateCredPair() (*protoJwk.Jwk, *protoKey.Key, error) {
	protoKeyMsg, err := wrapperKey.NewEd25519Key()
	if err != nil {
		return nil, nil, fmt.Errorf("generating Ed25519 key: %w", err)
	}
	protoKeyMsg.Created = time.Now().Unix()

	immKey, err := wrapperKey.NewImmutableKey(protoKeyMsg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating immutable key: %w", err)
	}

	pub := immKey.PrivateKey().(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	jwk := &protoJwk.Jwk{
		Kid:     protoKeyMsg.Id,
		Alg:     "EdDSA",
		Kty:     protoKey.KeyType_KEY_TYPE_OKP,
		Crv:     "Ed25519",
		X:       base64.RawURLEncoding.EncodeToString(pub),
		Use:     protoJwk.JwkUseType_JWK_USE_TYPE_SIG,
		Created: time.Now().Unix(),
	}

	return jwk, protoKeyMsg, nil
}
