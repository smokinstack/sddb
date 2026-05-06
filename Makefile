.PHONY: all agent dashboard build clean install install-agent-service install-dashboard-service install-service

GOFLAGS := -trimpath
AGENT_OUT  := bin/sddb-agent
DASH_OUT   := bin/sddb-dashboard

all: build

build: agent dashboard

agent:
	@mkdir -p bin
	go build $(GOFLAGS) -o $(AGENT_OUT) ./cmd/agent

dashboard:
	@mkdir -p bin
	go build $(GOFLAGS) -o $(DASH_OUT) ./cmd/dashboard

# Cross-compile for Linux amd64 (useful if developing on Mac)
linux:
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o $(AGENT_OUT)-linux-amd64 ./cmd/agent
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o $(DASH_OUT)-linux-amd64 ./cmd/dashboard

# Install binaries to /usr/local/bin
install: build
	install -m 755 $(AGENT_OUT) /usr/local/bin/sddb-agent
	install -m 755 $(DASH_OUT)  /usr/local/bin/sddb-dashboard

clean:
	rm -rf bin/

run-agent:
	go run ./cmd/agent -addr :8484 -interval 5s

run-dashboard:
	go run ./cmd/dashboard -addr :8080 -poll 5s

# Install agent as a systemd service on this host.
# For mTLS, override ExecStart after install: sudo systemctl edit sddb-agent
install-agent-service: agent
	install -m 755 $(AGENT_OUT) /usr/local/bin/sddb-agent
	install -m 644 deploy/sddb-agent.service /etc/systemd/system/sddb-agent.service
	systemctl daemon-reload
	@echo ""
	@echo "Agent service installed."
	@echo "  Start now:  sudo systemctl enable --now sddb-agent"
	@echo "  Add mTLS:   sudo systemctl edit sddb-agent"
	@echo "              (add -tls-cert/-tls-key/-tls-ca to ExecStart)"
	@echo ""

# Install dashboard as a systemd service. Data is stored in /var/lib/sddb.
install-dashboard-service: dashboard
	install -m 755 $(DASH_OUT) /usr/local/bin/sddb-dashboard
	install -m 644 deploy/sddb-dashboard.service /etc/systemd/system/sddb-dashboard.service
	systemctl daemon-reload
	@echo ""
	@echo "Dashboard service installed. Next steps:"
	@echo "  Set admin:    sudo sddb-dashboard set-admin -data-dir /var/lib/sddb"
	@echo "  Enroll agent: sudo sddb-dashboard enroll <name> -data-dir /var/lib/sddb"
	@echo "  Start now:    sudo systemctl enable --now sddb-dashboard"
	@echo ""

# Install both services (agent + dashboard) on this machine.
install-service: install-agent-service install-dashboard-service
