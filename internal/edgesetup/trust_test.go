package edgesetup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileTLSProbeFailsClosedWithoutCAOrClock(t *testing.T) {
	probe := FileTLSProbe{CABundlePath: filepath.Join(t.TempDir(), "missing.pem")}
	status := probe.Readiness(context.Background())
	if status.Ready || status.CABundle || status.Clock || status.Reason == "" {
		t.Fatalf("缺少 CA/时钟时没有失败关闭: %#v", status)
	}
}

func TestFileTLSProbeRequiresBothCAAndClock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CaseScreen test CA"},
		NotBefore: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:  time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:      true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{unix: 1787798400}
	status := (FileTLSProbe{CABundlePath: path, Clock: clock}).Readiness(context.Background())
	if !status.Ready || !status.CABundle || !status.Clock || status.Reason != "" {
		t.Fatalf("CA 与时钟就绪状态错误: %#v", status)
	}
}
