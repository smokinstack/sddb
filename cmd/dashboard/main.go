package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/smokinstack/sddb/internal/ai"
	"github.com/smokinstack/sddb/internal/auth"
	"github.com/smokinstack/sddb/internal/config"
	"github.com/smokinstack/sddb/internal/dashboard"
	"github.com/smokinstack/sddb/internal/pki"
	"golang.org/x/term"
)

//go:embed web
var webFiles embed.FS

func main() {
	// Detect subcommands before flag parsing so we can use separate flag sets.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "set-admin":
			runSetAdmin(os.Args[2:])
			return
		case "reset-admin":
			runResetAdmin(os.Args[2:])
			return
		case "enroll":
			runEnroll(os.Args[2:])
			return
		case "help", "--help", "-h":
			printUsage()
			return
		}
	}

	runDashboard()
}

// ── set-admin ────────────────────────────────────────────────────────────────

func runSetAdmin(args []string) {
	fs := flag.NewFlagSet("set-admin", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory")
	fs.Parse(args)

	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	adminPath := filepath.Join(*dataDir, "admin.json")

	fmt.Print("Username: ")
	var username string
	fmt.Scanln(&username)
	if username == "" {
		fmt.Fprintln(os.Stderr, "username cannot be empty")
		os.Exit(1)
	}

	password := readPassword("Password: ")
	confirm := readPassword("Confirm password: ")
	if password != confirm {
		fmt.Fprintln(os.Stderr, "passwords do not match")
		os.Exit(1)
	}

	if err := auth.SetAdmin(adminPath, username, password); err != nil {
		log.Fatalf("set admin: %v", err)
	}
	fmt.Printf("\nAdmin account '%s' saved to %s\n", username, adminPath)
	fmt.Println("The dashboard will require login on next start.")
}

// ── reset-admin ───────────────────────────────────────────────────────────────

func runResetAdmin(args []string) {
	fs := flag.NewFlagSet("reset-admin", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory")
	fs.Parse(args)

	adminPath := filepath.Join(*dataDir, "admin.json")
	if _, err := os.Stat(adminPath); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "no admin account found — run 'set-admin' to create one")
		os.Exit(1)
	}

	fmt.Print("New username (leave blank to keep existing): ")
	var username string
	fmt.Scanln(&username)

	// Load existing to fall back on current username if blank
	existing, err := auth.Load(adminPath)
	if err != nil {
		log.Fatalf("load existing credentials: %v", err)
	}
	if username == "" && existing != nil {
		username = existing.Username
	}
	if username == "" {
		fmt.Fprintln(os.Stderr, "username cannot be empty")
		os.Exit(1)
	}

	password := readPassword("New password: ")
	confirm := readPassword("Confirm password: ")
	if password != confirm {
		fmt.Fprintln(os.Stderr, "passwords do not match")
		os.Exit(1)
	}

	if err := auth.SetAdmin(adminPath, username, password); err != nil {
		log.Fatalf("reset admin: %v", err)
	}
	fmt.Printf("\nCredentials reset for '%s'.\n", username)
	fmt.Println("Restart the dashboard to apply.")
}

func readPassword(prompt string) string {
	fmt.Print(prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		log.Fatalf("read password: %v", err)
	}
	return string(b)
}

// ── enroll ───────────────────────────────────────────────────────────────────

func runEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory")
	outDir := fs.String("out", ".", "directory to write agent cert files into")

	// Separate the positional name arg from flags so they can appear in any order.
	var name string
	var flagArgs []string
	for _, a := range args {
		if len(a) > 0 && a[0] != '-' && name == "" {
			name = a
		} else {
			flagArgs = append(flagArgs, a)
		}
	}
	fs.Parse(flagArgs)

	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: sddb-dashboard enroll <name> [flags]")
		fmt.Fprintln(os.Stderr, "  <name> is a label for the agent (used as the output filename prefix)")
		os.Exit(1)
	}

	ca, err := pki.LoadOrCreate(*dataDir)
	if err != nil {
		log.Fatalf("PKI: %v", err)
	}

	// Ensure the dashboard client cert also exists (generated once, reused).
	ensureDashboardCert(*dataDir, ca)

	certPEM, keyPEM, err := ca.IssueCert(pki.AgentCN)
	if err != nil {
		log.Fatalf("issue cert: %v", err)
	}

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		log.Fatalf("create out dir: %v", err)
	}

	certFile := filepath.Join(*outDir, name+"-agent.crt")
	keyFile := filepath.Join(*outDir, name+"-agent.key")
	caFile := filepath.Join(*outDir, name+"-ca.crt")

	if err := os.WriteFile(certFile, certPEM, 0644); err != nil {
		log.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		log.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(caFile, ca.CertPEM, 0644); err != nil {
		log.Fatalf("write ca: %v", err)
	}

	fmt.Printf("\nAgent certificate issued for '%s'\n\n", name)
	fmt.Printf("Files written to %s:\n", *outDir)
	fmt.Printf("  %-30s  agent TLS certificate\n", name+"-agent.crt")
	fmt.Printf("  %-30s  agent TLS private key\n", name+"-agent.key")
	fmt.Printf("  %-30s  CA cert (agent uses this to verify the dashboard)\n\n", name+"-ca.crt")
	fmt.Println("Copy these to the agent host and run:")
	fmt.Printf("  scp %s %s %s user@<host>:/etc/sddb/\n\n", certFile, keyFile, caFile)
	fmt.Println("Then start the agent with:")
	fmt.Printf("  sddb-agent \\\n")
	fmt.Printf("    -tls-cert /etc/sddb/%s-agent.crt \\\n", name)
	fmt.Printf("    -tls-key  /etc/sddb/%s-agent.key \\\n", name)
	fmt.Printf("    -tls-ca   /etc/sddb/%s-ca.crt\n\n", name)
	fmt.Println("Restart the dashboard to activate mTLS for all agents.")
}

// ── dashboard server ──────────────────────────────────────────────────────────

func runDashboard() {
	addr := flag.String("addr", ":8080", "dashboard listen address")
	pollInterval := flag.Duration("poll", 5*time.Second, "agent poll interval")
	agentPort := flag.Int("agent-port", 8484, "default agent port for network scan and bare-IP adds")
	dataDir := flag.String("data-dir", defaultDataDir(), "data directory (stores PKI and agent list)")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	// ── Auth ──────────────────────────────────────────────────────────────────
	creds, err := auth.Load(filepath.Join(*dataDir, "admin.json"))
	if err != nil {
		log.Fatalf("load admin credentials: %v", err)
	}
	var sessions *auth.Sessions
	if creds != nil {
		sessions = auth.NewSessions()
		log.Printf("admin login enabled for user '%s'", creds.Username)
	} else {
		log.Println("WARNING: no admin account set — dashboard is unprotected. Run 'sddb-dashboard set-admin' to secure it.")
	}

	// ── PKI / TLS ─────────────────────────────────────────────────────────────
	var clientTLS *pki.CA
	caPath := filepath.Join(*dataDir, "ca.crt")
	if _, err := os.Stat(caPath); err == nil {
		ca, err := pki.LoadOrCreate(*dataDir)
		if err != nil {
			log.Fatalf("load CA: %v", err)
		}
		clientTLS = ca
		log.Println("mTLS enabled — agents must present a certificate signed by the dashboard CA")
	}

	aiClient := ai.New(
		os.Getenv("ANTHROPIC_API_KEY"),
		os.Getenv("OPENAI_API_KEY"),
		os.Getenv("OLLAMA_BASE_URL"),
		os.Getenv("OLLAMA_MODEL"),
	)
	if aiClient.Available() {
		log.Printf("AI assistant enabled — %s", aiClient.Provider())
	}

	dashCfg := dashboard.Config{
		AgentPort: *agentPort,
		DataDir:   *dataDir,
		Creds:     creds,
		Sessions:  sessions,
		AI:        aiClient,
	}

	if clientTLS != nil {
		dashCertPEM, dashKeyPEM, tlsErr := loadOrCreateDashboardCert(*dataDir, clientTLS)
		if tlsErr != nil {
			log.Fatalf("dashboard cert: %v", tlsErr)
		}
		tlsCfg, tlsErr := clientTLS.DashboardClientTLS(dashCertPEM, dashKeyPEM)
		if tlsErr != nil {
			log.Fatalf("tls config: %v", tlsErr)
		}
		dashCfg.TLS = tlsCfg
	}

	// ── Config + State + Poller + Dashboard ──────────────────────────────────
	cfg, err := config.Load(*dataDir)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	persistPath := filepath.Join(*dataDir, "agents.json")
	state := dashboard.NewState(persistPath)
	notify := make(chan struct{}, 16)
	poller := dashboard.NewPoller(state, *pollInterval, notify, dashCfg.TLS, cfg)

	dashCfg.Cfg = cfg

	webFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	dash, err := dashboard.NewDashboard(state, poller, notify, webFS, dashCfg)
	if err != nil {
		log.Fatalf("dashboard: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go poller.Run(ctx)

	srv := &http.Server{
		Addr:    *addr,
		Handler: dash.Handler(),
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	log.Printf("dashboard listening on %s", *addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sddb"
	}
	return filepath.Join(home, ".sddb")
}

func ensureDashboardCert(dataDir string, ca *pki.CA) {
	_, _, _ = loadOrCreateDashboardCert(dataDir, ca)
}

func loadOrCreateDashboardCert(dataDir string, ca *pki.CA) (certPEM, keyPEM []byte, err error) {
	certPath := filepath.Join(dataDir, "dashboard.crt")
	keyPath := filepath.Join(dataDir, "dashboard.key")

	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		certPEM, keyPEM, err = ca.IssueCert(pki.DashboardCN)
		if err != nil {
			return nil, nil, fmt.Errorf("issue dashboard cert: %w", err)
		}
		if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
			return nil, nil, err
		}
		log.Println("Generated dashboard client certificate")
		return certPEM, keyPEM, nil
	}

	certPEM, err = os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = os.ReadFile(keyPath)
	return certPEM, keyPEM, err
}

func printUsage() {
	fmt.Println(`sddb-dashboard — container dashboard server

Usage:
  sddb-dashboard [flags]              start the dashboard
  sddb-dashboard set-admin [flags]    create or update the admin account
  sddb-dashboard reset-admin [flags]  reset the admin password
  sddb-dashboard enroll <name>        issue an mTLS certificate for an agent

Dashboard flags:
  -addr        listen address (default :8080)
  -poll        agent poll interval (default 5s)
  -agent-port  default agent port (default 8484)
  -data-dir    data directory for PKI and config (default ~/.sddb)

set-admin flags:
  -data-dir    same as above

enroll flags:
  -data-dir    same as above
  -out         directory to write agent cert files (default .)`)
}
