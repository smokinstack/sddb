.PHONY: all agent dashboard build clean install

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
