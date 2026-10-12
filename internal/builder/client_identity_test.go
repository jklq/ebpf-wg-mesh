package builder

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
)

func TestBuilderPresentsRenewedClientIdentityWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	issue := func(serial int64, dnsName string, usage x509.ExtKeyUsage) ([]byte, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "builder"}, DNSNames: []string{dnsName}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, key.Public(), caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	firstCert, firstKey := issue(2, "builder", x509.ExtKeyUsageClientAuth)
	write("client.crt", firstCert)
	write("client.key", firstKey)
	serverCert, serverKey := issue(10, "controlplane.test", x509.ExtKeyUsageServerAuth)
	serverPair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	presented := make(chan int64, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conn := c.(*tls.Conn)
			if err := conn.Handshake(); err == nil {
				presented <- conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
			}
			conn.Close()
		}
	}()
	creds, err := loadTransportCredentials(config.InternalClientTLSConfig{CAFile: filepath.Join(dir, "ca.crt"), CertFile: filepath.Join(dir, "client.crt"), KeyFile: filepath.Join(dir, "client.key"), ServerName: "controlplane.test"})
	if err != nil {
		t.Fatal(err)
	}
	handshake := func() int64 {
		t.Helper()
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, _, err := creds.ClientHandshake(ctx, "controlplane.test", raw)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		select {
		case serial := <-presented:
			return serial
		case <-ctx.Done():
			t.Fatal("server did not observe the client identity")
			return 0
		}
	}
	if serial := handshake(); serial != 2 {
		t.Fatalf("initial identity serial %d", serial)
	}
	renewedCert, renewedKey := issue(3, "builder", x509.ExtKeyUsageClientAuth)
	write("client.crt", renewedCert)
	write("client.key", renewedKey)
	if serial := handshake(); serial != 3 {
		t.Fatalf("new connection presented serial %d instead of the renewed identity", serial)
	}
	nextCert, _ := issue(4, "builder", x509.ExtKeyUsageClientAuth)
	write("client.crt", nextCert)
	if serial := handshake(); serial != 3 {
		t.Fatalf("mismatched pair presented serial %d", serial)
	}
}
