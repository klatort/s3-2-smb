.PHONY: build clean install uninstall test update upgrade

# Binary name
BINARY=s3smb-gateway
BUILD_DIR=build

# Version info
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/s3smb-gateway

test:
	go test -v ./...

clean:
	rm -rf $(BUILD_DIR)

install: build
	@echo "Stopping service if running..."
	-sudo systemctl stop s3smb-gateway 2>/dev/null || true
	
	@echo "Installing binary and configuration..."
	sudo cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/
	sudo mkdir -p /etc/s3smb-gateway /var/cache/s3smb-gateway /var/lib/s3smb-gateway
	sudo cp -n config.example.json /etc/s3smb-gateway/config.json || true
	
	@echo "Installing systemd service..."
	@printf "[Unit]\nDescription=S3SMB Gateway FUSE Filesystem\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nExecStart=/usr/local/bin/s3smb-gateway -config /etc/s3smb-gateway/config.json\nExecStop=/bin/fusermount -u /mnt/s3\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=multi-user.target\n" > $(BUILD_DIR)/s3smb-gateway.service
	sudo cp $(BUILD_DIR)/s3smb-gateway.service /etc/systemd/system/
	
	-sudo systemctl daemon-reload 2>/dev/null || true
	@echo "Starting service..."
	-sudo systemctl enable --now s3smb-gateway 2>/dev/null || true
	@echo "Flushing Samba internal caches..."
	-sudo systemctl restart smbd 2>/dev/null || echo "Warning: Could not restart smbd"
	@echo "Install complete! Binary is at /usr/local/bin/$(BINARY)"

uninstall:
	@echo "Stopping and disabling service..."
	-sudo systemctl stop s3smb-gateway 2>/dev/null || true
	-sudo systemctl disable s3smb-gateway 2>/dev/null || true
	sudo rm -f /etc/systemd/system/s3smb-gateway.service
	-sudo systemctl daemon-reload 2>/dev/null || true
	sudo rm -f /usr/local/bin/$(BINARY)
	@echo "Uninstall complete."

update:
	@echo "Pulling latest changes and reinstalling..."
	@if [ -d .git ]; then \
		git pull; \
		go mod download; \
		make install; \
		echo "Update complete."; \
	else \
		echo "Not a git repository. Cannot update automatically."; \
	fi

upgrade: update

help:
	@echo "Available targets:"
	@echo "  build      - Compile the Go binary"
	@echo "  test       - Run tests"
	@echo "  install    - Install binary, config, systemd service, and start it"
	@echo "  uninstall  - Stop service and remove all installed files"
	@echo "  update     - Pull latest git changes, compile, and reinstall"
	@echo "  clean      - Remove build artifacts"
