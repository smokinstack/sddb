package pki

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type pair struct{ cert, key []byte }

func mustIssue(t *testing.T, ca *CA, cn string, usages ...x509.ExtKeyUsage) pair {
	t.Helper()
	c, k, err := ca.issue(cn, usages...)
	if err != nil {
		t.Fatal(err)
	}
	return pair{c, k}
}

// handshake runs a full mutual-TLS handshake over an in-memory connection and
// returns the server's verdict (in TLS 1.3 the server checks the client
// certificate after the client has already finished).
func handshake(t *testing.T, server, client *tls.Config) error {
	t.Helper()
	sc, cc := net.Pipe()
	defer sc.Close()
	defer cc.Close()

	serverErr := make(chan error, 1)
	go func() {
		s := tls.Server(sc, server)
		err := s.Handshake()
		if err == nil {
			// Wait for the client's ping so post-handshake messages flow.
			buf := make([]byte, 4)
			_, err = io.ReadFull(s, buf)
		}
		serverErr <- err
	}()

	c := tls.Client(cc, client)
	if err := c.Handshake(); err != nil {
		return err
	}
	go c.Write([]byte("ping"))
	go io.Copy(io.Discard, c) // drain alerts so the server never blocks

	select {
	case err := <-serverErr:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("handshake timed out")
		return nil
	}
}

func setup(t *testing.T) (*CA, map[string]pair) {
	t.Helper()
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	both := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	certs := map[string]pair{
		// Issued the way enroll did before this change (both usages).
		"legacy-agent":     mustIssue(t, ca, AgentCN, both...),
		"legacy-dashboard": mustIssue(t, ca, DashboardCN, both...),
	}
	var p pair
	if p.cert, p.key, err = ca.IssueServerCert(AgentCN); err != nil {
		t.Fatal(err)
	}
	certs["agent"] = p
	if p.cert, p.key, err = ca.IssueClientCert(DashboardCN); err != nil {
		t.Fatal(err)
	}
	certs["dashboard"] = p
	return ca, certs
}

func TestAgentAcceptsOnlyTheDashboard(t *testing.T) {
	ca, certs := setup(t)

	for _, serverCert := range []string{"legacy-agent", "agent"} {
		srv, err := AgentServerTLS(certs[serverCert].cert, certs[serverCert].key, ca.CertPEM)
		if err != nil {
			t.Fatal(err)
		}

		cases := []struct {
			client  string
			wantErr string // "" = must succeed
		}{
			{"dashboard", ""},
			{"legacy-dashboard", ""}, // existing installs keep working without re-enrolling
			{"legacy-agent", "not the dashboard"},
			{"agent", "certificate"}, // server-only cert is refused as a client cert
		}
		for _, tc := range cases {
			t.Run(serverCert+"/"+tc.client, func(t *testing.T) {
				cli, err := ca.DashboardClientTLS(certs[tc.client].cert, certs[tc.client].key)
				if err != nil {
					t.Fatal(err)
				}
				err = handshake(t, srv, cli)
				switch {
				case tc.wantErr == "" && err != nil:
					t.Fatalf("handshake failed: %v", err)
				case tc.wantErr != "" && err == nil:
					t.Fatalf("an agent accepted a %s certificate as its client", tc.client)
				case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
				}
			})
		}
	}
}

func TestIssuedCertUsages(t *testing.T) {
	_, certs := setup(t)
	for name, want := range map[string]x509.ExtKeyUsage{
		"agent":     x509.ExtKeyUsageServerAuth,
		"dashboard": x509.ExtKeyUsageClientAuth,
	} {
		kp, err := tls.X509KeyPair(certs[name].cert, certs[name].key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(kp.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != want {
			t.Errorf("%s cert usages = %v, want only %v", name, leaf.ExtKeyUsage, want)
		}
	}
}
