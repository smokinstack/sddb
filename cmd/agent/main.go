package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jester/sddb/internal/agent"
	"github.com/jester/sddb/internal/pki"
)

func main() {
	addr := flag.String("addr", ":8484", "listen address")
	interval := flag.Duration("interval", 5*time.Second, "stats refresh interval")
	id := flag.String("id", "", "agent ID (auto-generated if empty)")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate file (enables mTLS)")
	tlsKey := flag.String("tls-key", "", "path to TLS private key file")
	tlsCA := flag.String("tls-ca", "", "path to CA certificate file (verifies dashboard identity)")
	flag.Parse()

	agentID := *id
	if agentID == "" {
		if v := os.Getenv("SDDB_AGENT_ID"); v != "" {
			agentID = v
		} else {
			agentID = uuid.New().String()
		}
	}

	docker, err := agent.NewDockerClient()
	if err != nil {
		log.Fatalf("docker: %v", err)
	}
	defer docker.Close()

	cfg := agent.Config{
		ID:             agentID,
		ListenAddr:     *addr,
		UpdateInterval: *interval,
	}

	if *tlsCert != "" || *tlsKey != "" || *tlsCA != "" {
		if *tlsCert == "" || *tlsKey == "" || *tlsCA == "" {
			log.Fatal("all three TLS flags must be provided together: -tls-cert, -tls-key, -tls-ca")
		}
		certPEM, err := os.ReadFile(*tlsCert)
		if err != nil {
			log.Fatalf("read tls-cert: %v", err)
		}
		keyPEM, err := os.ReadFile(*tlsKey)
		if err != nil {
			log.Fatalf("read tls-key: %v", err)
		}
		caPEM, err := os.ReadFile(*tlsCA)
		if err != nil {
			log.Fatalf("read tls-ca: %v", err)
		}
		tlsCfg, err := pki.AgentServerTLS(certPEM, keyPEM, caPEM)
		if err != nil {
			log.Fatalf("tls config: %v", err)
		}
		cfg.TLS = tlsCfg
	}

	a := agent.New(cfg, docker)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		log.Fatalf("agent: %v", err)
	}
}
