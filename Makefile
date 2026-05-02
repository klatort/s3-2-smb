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
	@echo "Installing binary and configuration..."
	sudo rm -f /usr/local/bin/$(BINARY)
	sudo cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/
	sudo mkdir -p /etc/s3smb-gateway /var/cache/s3smb-gateway /var/lib/s3smb-gateway
	sudo cp -n config.example.json /etc/s3smb-gateway/default.json || true
	
	@echo "Migrating legacy JSON configurations in /etc/s3smb-gateway/..."
	@for conf in /etc/s3smb-gateway/*.json; do \
		if [ -f "$$conf" ] && command -v jq >/dev/null 2>&1; then \
			echo "  Checking $$conf for missing new fields..."; \
			jq 'if has("sync_on_start") then . else .sync_on_start = true end | if has("sync_interval") then . else .sync_interval = 3600 end | if has("writeback_idle_time") then . else .writeback_idle_time = 300 end | if has("writeback_interval") then . else .writeback_interval = 60 end' "$$conf" > "$$conf.tmp" && sudo mv "$$conf.tmp" "$$conf"; \
		fi; \
	done
	
	@echo "Installing systemd template service..."
	@printf "[Unit]\nDescription=S3SMB Gateway FUSE Filesystem (%%I)\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nExecStartPre=-/bin/fusermount -uz /mnt/s3/%%i\nExecStartPre=-/bin/umount -l /mnt/s3/%%i\nExecStartPre=/bin/mkdir -p /var/cache/s3smb-gateway/%%i /var/lib/s3smb-gateway/%%i /mnt/s3/%%i\nExecStart=/usr/local/bin/s3smb-gateway -config /etc/s3smb-gateway/%%i.json -cache /var/cache/s3smb-gateway/%%i -db /var/lib/s3smb-gateway/%%i/metadata.db -mount /mnt/s3/%%i\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=multi-user.target\n" > $(BUILD_DIR)/s3smb-gateway@.service
	sudo cp $(BUILD_DIR)/s3smb-gateway@.service /etc/systemd/system/
	
	-sudo systemctl daemon-reload 2>/dev/null || true
	@echo "Restarting active services forcefully to clear stuck mounts..."
	-sudo systemctl stop "s3smb-gateway@*" 2>/dev/null || true
	-sudo systemctl start "s3smb-gateway@*" 2>/dev/null || true
	-sudo systemctl enable --now s3smb-gateway@default 2>/dev/null || true
	@echo "Flushing Samba internal caches..."
	-sudo systemctl restart smbd 2>/dev/null || echo "Warning: Could not restart smbd"
	@echo "Install complete! Binary is at /usr/local/bin/$(BINARY)"

uninstall:
	@echo "Stopping and disabling service..."
	-sudo systemctl stop s3smb-gateway 2>/dev/null || true
	-sudo systemctl disable s3smb-gateway 2>/dev/null || true
	-sudo systemctl stop s3smb-gateway@* 2>/dev/null || true
	-sudo systemctl disable s3smb-gateway@* 2>/dev/null || true
	sudo rm -f /etc/systemd/system/s3smb-gateway.service
	sudo rm -f /etc/systemd/system/s3smb-gateway@.service
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

clean-cache:
	@echo "Stopping gateway services..."
	-sudo systemctl stop "s3smb-gateway@*" 2>/dev/null || true
	@echo "Clearing chunk caches (safely keeping databases and pending uploads)..."
	-sudo find /var/cache/s3smb-gateway/ -type f -name "*.chunk" -delete 2>/dev/null || true
	@echo "Restarting gateway services..."
	-sudo systemctl start "s3smb-gateway@*" 2>/dev/null || true
	@echo "Cache cleared."

upgrade: update

help:
	@echo "Available targets:"
	@echo "  build       - Compile the Go binary"
	@echo "  test        - Run tests"
	@echo "  install     - Install binary, config, systemd service, and start it"
	@echo "  uninstall   - Stop service and remove all installed files"
	@echo "  update      - Pull latest git changes, compile, migrate configs, and reinstall"
	@echo "  clean-cache - Safely wipe local read caches to fix stuck mounts"
	@echo "  clean       - Remove build artifacts"
