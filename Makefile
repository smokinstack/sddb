.PHONY: all agent dashboard build linux linux-arm64 release clean install install-agent-service install-dashboard-service install-service enroll

GOFLAGS   := -trimpath
AGENT_OUT := bin/sddb-agent
DASH_OUT  := bin/sddb-dashboard

# enroll target defaults — override on the command line as needed
DATA_DIR  ?= /var/lib/sddb
SSH_USER  ?= $(shell id -un)

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

linux-arm64:
	@mkdir -p bin
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o $(AGENT_OUT)-linux-arm64 ./cmd/agent
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o $(DASH_OUT)-linux-arm64 ./cmd/dashboard

# Build all four release platforms (linux/darwin × amd64/arm64)
release:
	@mkdir -p bin
	GOOS=linux  GOARCH=amd64 go build $(GOFLAGS) -o $(AGENT_OUT)-linux-amd64   ./cmd/agent
	GOOS=linux  GOARCH=amd64 go build $(GOFLAGS) -o $(DASH_OUT)-linux-amd64    ./cmd/dashboard
	GOOS=linux  GOARCH=arm64 go build $(GOFLAGS) -o $(AGENT_OUT)-linux-arm64   ./cmd/agent
	GOOS=linux  GOARCH=arm64 go build $(GOFLAGS) -o $(DASH_OUT)-linux-arm64    ./cmd/dashboard
	GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -o $(AGENT_OUT)-darwin-amd64  ./cmd/agent
	GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -o $(DASH_OUT)-darwin-amd64   ./cmd/dashboard
	GOOS=darwin GOARCH=arm64 go build $(GOFLAGS) -o $(AGENT_OUT)-darwin-arm64  ./cmd/agent
	GOOS=darwin GOARCH=arm64 go build $(GOFLAGS) -o $(DASH_OUT)-darwin-arm64   ./cmd/dashboard

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
	sudo install -m 755 $(AGENT_OUT) /usr/local/bin/sddb-agent
	sudo install -m 644 deploy/sddb-agent.service /etc/systemd/system/sddb-agent.service
	sudo systemctl daemon-reload
	@echo ""
	@echo "Agent service installed."
	@echo "  Start now:  sudo systemctl enable --now sddb-agent"
	@echo "  Add mTLS:   sudo systemctl edit sddb-agent"
	@echo "              (add -tls-cert/-tls-key/-tls-ca to ExecStart)"
	@echo ""

# Install dashboard as a systemd service. Data is stored in /var/lib/sddb.
install-dashboard-service: dashboard
	sudo install -m 755 $(DASH_OUT) /usr/local/bin/sddb-dashboard
	sudo install -m 644 deploy/sddb-dashboard.service /etc/systemd/system/sddb-dashboard.service
	sudo systemctl daemon-reload
	@echo ""
	@echo "Dashboard service installed. Next steps:"
	@echo "  Set admin:    sudo sddb-dashboard set-admin -data-dir /var/lib/sddb"
	@echo "  Enroll agent: sudo sddb-dashboard enroll <name> -data-dir /var/lib/sddb"
	@echo "  Start now:    sudo systemctl enable --now sddb-dashboard"
	@echo ""

# Install both services (agent + dashboard) on this machine.
install-service: install-agent-service install-dashboard-service

# Enroll a new agent and push its certificates to the agent host over SSH.
# Usage: make enroll HOST=myserver [SSH_USER=ubuntu] [DATA_DIR=/var/lib/sddb]
#
# Requires:
#   - sddb-dashboard installed and data initialised on this host
#   - SSH access to HOST (key-based auth recommended)
#   - The sddb user already exists on HOST (created by deploy/install-agent.sh)
enroll:
	@test -n "$(HOST)" || (echo "Usage: make enroll HOST=<label> [SSH_USER=<user>]" && false)
	$(eval ENROLL_TMP := $(shell mktemp -d))
	@echo "==> Enrolling certificate for $(HOST) ..."
	sudo sddb-dashboard enroll $(HOST) -data-dir $(DATA_DIR) -out $(ENROLL_TMP)
	@echo "==> Pushing certificates to $(SSH_USER)@$(HOST) ..."
	ssh $(SSH_USER)@$(HOST) "sudo mkdir -p /etc/sddb"
	< $(ENROLL_TMP)/$(HOST)-agent.crt ssh $(SSH_USER)@$(HOST) "sudo tee /etc/sddb/agent.crt > /dev/null"
	< $(ENROLL_TMP)/$(HOST)-agent.key ssh $(SSH_USER)@$(HOST) "sudo tee /etc/sddb/agent.key > /dev/null"
	< $(ENROLL_TMP)/$(HOST)-ca.crt    ssh $(SSH_USER)@$(HOST) "sudo tee /etc/sddb/ca.crt    > /dev/null"
	@echo "==> Setting ownership and permissions on $(HOST) ..."
	ssh $(SSH_USER)@$(HOST) " \
	  sudo chown sddb:sddb /etc/sddb/agent.crt /etc/sddb/agent.key /etc/sddb/ca.crt && \
	  sudo chmod 640 /etc/sddb/agent.crt /etc/sddb/ca.crt && \
	  sudo chmod 600 /etc/sddb/agent.key"
	@rm -rf $(ENROLL_TMP)
	@echo ""
	@echo "Done. Certificate for '$(HOST)' is installed."
	@echo "  If this is the first enrolled agent, restart the dashboard to activate mTLS:"
	@echo "    sudo systemctl restart sddb-dashboard"
	@echo "  Start the agent on $(HOST):"
	@echo "    ssh $(SSH_USER)@$(HOST) 'sudo systemctl enable --now sddb-agent'"
	@echo ""
