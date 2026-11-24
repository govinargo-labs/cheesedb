package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// selfSigned returns the certificate and key files in dir, making a
// self-signed pair there first if dir has none. It is for containers: the
// server must listen on the container's address, so it needs TLS, and a
// person trying it out has no certificate yet. Clients trust tls.crt itself
// (curl --cacert tls.crt); there is no certificate authority behind it.
//
// The pair is kept and reused, so clients that trust it keep working
// across restarts. hosts are extra names and addresses it is valid for,
// such as a container's DNS name; it always covers localhost, 127.0.0.1
// and ::1. To change them, delete the two files.
func selfSigned(dir string, hosts []string, out io.Writer) (certFile, keyFile string, err error) {
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	_, certErr := os.Stat(certFile)
	_, keyErr := os.Stat(keyFile)
	switch {
	case certErr == nil && keyErr == nil:
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return "", "", fmt.Errorf("--tls-auto %s: %w (delete tls.crt and tls.key to make new ones)", dir, err)
		}
		if exp := pair.Leaf.NotAfter; time.Now().After(exp) {
			return "", "", fmt.Errorf("--tls-auto %s: the certificate expired on %s; delete tls.crt and tls.key to make new ones", dir, exp.Format(time.DateOnly))
		}
		describeCert(out, certFile, pair.Leaf, "using")
		return certFile, keyFile, nil
	case !errors.Is(certErr, fs.ErrNotExist) && certErr != nil:
		return "", "", certErr
	case !errors.Is(keyErr, fs.ErrNotExist) && keyErr != nil:
		return "", "", keyErr
	}
	// None, or only one of the two (a crash while making them): make both.

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "cheesedb"},
		NotBefore:    now.Add(-time.Hour), // a little clock skew between machines
		NotAfter:     now.AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// It signs itself, so it is its own authority: clients that are
		// given it as the one to trust (--cacert) accept it.
		IsCA:                  true,
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	// Being its own authority, it could sign a certificate for any website,
	// and a machine that trusts it would believe that certificate. Name
	// constraints limit it to its own names and addresses, and it may sign
	// no further authorities, so tls.key in the wrong hands cannot be used
	// to impersonate other sites.
	tmpl.MaxPathLenZero = true
	tmpl.PermittedDNSDomainsCritical = true
	for _, name := range tmpl.DNSNames {
		tmpl.PermittedDNSDomains = append(tmpl.PermittedDNSDomains, strings.TrimPrefix(name, "*."))
	}
	for _, ip := range tmpl.IPAddresses {
		bits := 8 * len(ip)
		if ip4 := ip.To4(); ip4 != nil {
			ip, bits = ip4, 32
		}
		tmpl.PermittedIPRanges = append(tmpl.PermittedIPRanges, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	// The key first: tls.crt appearing last marks the pair complete.
	if err := writePEM(keyFile, "PRIVATE KEY", keyDER, 0o600); err != nil {
		return "", "", err
	}
	if err := writePEM(certFile, "CERTIFICATE", der, 0o644); err != nil {
		return "", "", err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return "", "", err
	}
	describeCert(out, certFile, leaf, "made a self-signed")
	return certFile, keyFile, nil
}

// writePEM writes one PEM block through a temporary file and a rename, so
// a crash never leaves half a file.
func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// describeCert prints what a client needs to trust the certificate: where
// it is, its fingerprint (to compare with what a client sees), its names
// and when it expires.
func describeCert(out io.Writer, path string, c *x509.Certificate, verb string) {
	sum := sha256.Sum256(c.Raw)
	names := append([]string{}, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	fmt.Fprintf(out, "TLS: %s certificate %s\n  for %s, valid until %s\n  SHA-256 fingerprint %X\n  clients must trust this file (curl --cacert %s)\n",
		verb, path, strings.Join(names, ", "), c.NotAfter.Format(time.DateOnly), sum, path)
}

// splitList splits a comma-separated flag value, dropping empty parts.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
