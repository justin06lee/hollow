# hollow — computers for bots.
#
#   make            build hollow with the guest agent inside it, install it, prove it runs
#   make build      just produce ./build/hollow
#   make install    put hollow on $PATH
#   make update     stop a running host, reinstall, start it again
#
#   make service    run the host as a systemd service, surviving reboots (Linux)
#   make ship HOST=root@box   build for linux/amd64 and install on that machine over ssh

BINARY  := hollow
BUILD   := build
AGENTS  := internal/host/agents
# --match 'v*' so a feature tag never becomes the version.
VERSION := $(shell git describe --tags --match 'v*' --dirty --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# /usr/local/bin when this machine lets us write there, ~/.local/bin otherwise.
PREFIX ?= $(shell if [ -w /usr/local/bin ]; then echo /usr/local; else echo $(HOME)/.local; fi)
BINDIR := $(PREFIX)/bin

# Command lines of the hosts `make update` stopped, so it can start them again.
RESTART := .make-restart

.PHONY: all agent build build-linux install uninstall update service unservice ship \
        test race fmt vet check clean stop-host start-host check-path

all: build install check-path
	@echo
	@echo "  hollow $(VERSION) installed to $(BINDIR)/$(BINARY)"
	@echo
	@echo "  run the host:      hollow serve"
	@echo "  build the image:   hollow pull linux"
	@echo "  boot a desk:       hollow new"
	@echo

# The agent runs inside the guest, so it is always built for the guest's
# platform, whatever this machine is, and embedded into the host binary. The
# host hands it to every desk at boot, which is why the guest image never has
# to be rebuilt for a new agent.
agent:
	@mkdir -p $(AGENTS)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(AGENTS)/hollow-agent-linux-amd64 ./cmd/hollow-agent

build: agent
	@mkdir -p $(BUILD)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BUILD)/$(BINARY) ./cmd/hollow

# The host binary for the machine that runs the VMs, which is Linux.
build-linux: agent
	@mkdir -p $(BUILD)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(BUILD)/$(BINARY)-linux-amd64 ./cmd/hollow

install: build
	@mkdir -p $(BINDIR)
	install -m 0755 $(BUILD)/$(BINARY) $(BINDIR)/$(BINARY)

uninstall:
	@rm -f $(BINDIR)/$(BINARY)

# Refresh a live install: stop the hosts running the old binary, replace it,
# and start them again with the arguments they had. A systemd service is
# restarted rather than re-run by hand.
update: stop-host uninstall build install start-host check-path
	@echo "  hollow $(VERSION) is live"

stop-host:
	@if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet hollow 2>/dev/null; then \
		echo "  stopping the hollow service"; sudo systemctl stop hollow; echo service > $(RESTART); \
	else \
		ps -axo pid=,args= 2>/dev/null | sed 's/^ *//' | grep '[h]ollow serve' > $(RESTART) || true; \
		while read -r pid args; do echo "  stopping host $$pid"; kill "$$pid" 2>/dev/null || true; done < $(RESTART); \
	fi

start-host:
	@if [ "$$(cat $(RESTART) 2>/dev/null)" = service ]; then \
		echo "  starting the hollow service"; sudo systemctl start hollow; \
	else \
		while read -r pid args; do echo "  restarting: $$args"; nohup sh -c "$$args" >/dev/null 2>&1 & done < $(RESTART); \
	fi
	@rm -f $(RESTART)

check-path:
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) \
		echo; echo "  $(BINDIR) is not on your PATH; add it:"; echo "      export PATH=\"$(BINDIR):\$$PATH\"";; esac

# The host as a service: its own user, in the kvm group, state under
# /var/lib/hollow. Kept out of `make` on purpose — installing something that
# starts at boot is a bigger thing to do to a machine than copying a binary.
service: install
	@if [ "$$(uname -s)" != Linux ]; then echo "  make service installs a systemd unit, and this machine is not Linux."; exit 1; fi
	@id hollow >/dev/null 2>&1 || sudo useradd --system --home-dir /var/lib/hollow --shell /usr/sbin/nologin hollow
	@sudo usermod -aG kvm hollow
	@sed 's|@BINDIR@|$(BINDIR)|g' dist/hollow.service | sudo tee /etc/systemd/system/hollow.service >/dev/null
	@sudo systemctl daemon-reload
	@sudo systemctl enable --now hollow
	@sleep 1
	@echo "  hollow is running as a service; its connect code:"
	@sudo HOLLOW_HOME=/var/lib/hollow $(BINDIR)/$(BINARY) connect | sed 's/^/      /'
	@echo "  logs:  journalctl -u hollow -f"

unservice:
	@sudo systemctl disable --now hollow 2>/dev/null || true
	@sudo rm -f /etc/systemd/system/hollow.service
	@sudo systemctl daemon-reload

# Build here, install there. The box that runs VMs is often not the machine
# the code is edited on.
ship: build-linux
	@test -n "$(HOST)" || { echo "usage: make ship HOST=root@box"; exit 1; }
	@echo "  ship $(BINARY) $(VERSION) -> $(HOST)"
	@ssh "$(HOST)" 'cat > /tmp/hollow.new && install -m 0755 /tmp/hollow.new /usr/local/bin/hollow && rm -f /tmp/hollow.new && \
		if systemctl is-active --quiet hollow 2>/dev/null; then systemctl restart hollow && echo "  restarted the hollow service"; fi && \
		hollow version' < $(BUILD)/$(BINARY)-linux-amd64

check: fmt vet test race

test:
	@go test ./...

race:
	@go test -race ./... >/dev/null && echo "  race ok"

fmt:
	@gofmt -l . | grep -v '^$$' && { echo "unformatted files above"; exit 1; } || echo "  fmt ok"

vet:
	@go vet ./... && echo "  vet ok"

clean:
	@rm -rf $(BUILD) $(AGENTS)/hollow-agent-* $(RESTART)
