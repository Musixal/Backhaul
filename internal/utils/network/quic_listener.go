package network

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

func QUICListen(address string, tlsCertFile, tlsKeyFile string) (*quic.Listener, error) {
	var tlsConfig *tls.Config
	if tlsCertFile != "" && tlsKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(tlsCertFile, tlsKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load TLS certificate: %v", err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"backhaul-quic"},
		}
	} else {
		tlsConfig = generateSelfSignedCert()
	}

	quicConfig := &quic.Config{
		MaxIdleTimeout:        60 * time.Second,
		MaxIncomingStreams:    1000,
		MaxIncomingUniStreams: 1000,
		KeepAlivePeriod:       10 * time.Second,
		HandshakeIdleTimeout:  10 * time.Second,
	}

	listener, err := quic.ListenAddr(address, tlsConfig, quicConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to start QUIC listener: %v", err)
	}

	return listener, nil
}

func generateSelfSignedCert() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{generateCert()},
		NextProtos:   []string{"backhaul-quic"},
	}
}

func generateCert() tls.Certificate {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Backhaul QUIC"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}
	}

	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
	}
}
