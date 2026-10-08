package productionops

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"
)

func inspectClientIdentity(caFile, certFile, keyFile, id, class string) (string, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return "", fmt.Errorf("invalid client authority")
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return "", err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", err
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return "", err
		}
		intermediates.AddCert(cert)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", fmt.Errorf("client certificate is not currently trusted")
	}
	if leaf.Subject.CommonName != id || len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] != class || time.Until(leaf.NotAfter) < 5*time.Minute || leaf.SerialNumber.Sign() <= 0 {
		return "", fmt.Errorf("client identity or expiry differs")
	}
	return leaf.SerialNumber.Text(16), nil
}
