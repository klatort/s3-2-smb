.PHONY: build clean install test run

# Binary name
BINARY=s3smb-gateway

# Build directory
BUILD_DIR=build

# Version info
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Go build flags
LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

# Default target
all: build

# Build the binary
build:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/s3smb-gateway

# Build with debug info
build-debug:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -gcflags="all=-N -l" -o $(BUILD_DIR)/$(BINARY) ./cmd/s3smb-gateway

# Install dependencies
deps:
	go mod download
	go mod tidy

# Run tests
test:
	go test -v ./...

# Run tests with coverage
test-coverage:
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Run linter
lint:
	golangci-lint run ./...

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

# Install to system
install: build
	sudo cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/
	sudo mkdir -p /etc/s3smb-gateway
	sudo cp config.example.json /etc/s3smb-gateway/config.json.example
	@echo "Installed $(BINARY) to /usr/local/bin/"

# Uninstall from system
uninstall:
	sudo rm -f /usr/local/bin/$(BINARY)
	@echo "Uninstalled $(BINARY)"

# Create directories
setup-dirs:
	sudo mkdir -p /var/cache/s3smb-gateway
	sudo mkdir -p /var/lib/s3smb-gateway
	sudo chown $(USER):$(USER) /var/cache/s3smb-gateway
	sudo chown $(USER):$(USER) /var/lib/s3smb-gateway

# Run locally (for development)
run: build
	./$(BUILD_DIR)/$(BINARY) $(ARGS)

# Build for multiple platforms
build-all:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-amd64 ./cmd/s3smb-gateway
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-arm64 ./cmd/s3smb-gateway

# Generate systemd service file
systemd:
	@echo "[Unit]" > $(BUILD_DIR)/s3smb-gateway.service
	@echo "Description=S3SMB Gateway FUSE Filesystem" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "After=network-online.target" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "Wants=network-online.target" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "[Service]" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "Type=simple" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "ExecStart=/usr/local/bin/s3smb-gateway -config /etc/s3smb-gateway/config.json" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "ExecStop=/bin/fusermount -u /mnt/s3" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "Restart=on-failure" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "RestartSec=5" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "[Install]" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "WantedBy=multi-user.target" >> $(BUILD_DIR)/s3smb-gateway.service
	@echo "Generated $(BUILD_DIR)/s3smb-gateway.service"

# Install systemd service
install-service: systemd
	sudo cp $(BUILD_DIR)/s3smb-gateway.service /etc/systemd/system/
	sudo systemctl daemon-reload
	@echo "Installed systemd service. Enable with: sudo systemctl enable s3smb-gateway"

# Check for available updates
check-update:
	@echo "Checking for updates..."
	@if [ -d .git ]; then \
		git fetch origin; \
		LOCAL=$(git rev-parse @); \
		REMOTE=$(git rev-parse @{u}); \
		BASE=$(git merge-base @ @{u}); \
		if [ "$$LOCAL" = "$$REMOTE" ]; then \
			echo "Already up-to-date"; \
		elif [ "$$LOCAL" = "$$BASE" ]; then \
			echo "Updates available. Run 'make update' to pull changes."; \
		elif [ "$$REMOTE" = "$$BASE" ]; then \
			echo "Local changes not pushed. Run 'git push' to push changes."; \
		else \
			echo "Diverged from remote. Consider 'git pull --rebase'."; \
		fi; \
	else \
		echo "Not a git repository. Cannot check for updates."; \
	fi

# Update dependencies to latest compatible versions
update-deps:
	@echo "Updating dependencies..."
	go get -u ./...
	go mod tidy

# Update application from git repository
update:
	@echo "Updating application from git..."
	@if [ -d .git ]; then \
		git pull; \
		make deps; \
		make build; \
		echo "Update completed successfully."; \
	else \
		echo "Not a git repository. Cannot update."; \
	fi

# Upgrade application (reinstall after update)
upgrade: update install
	@echo "Upgrade completed. Application reinstalled."

help:
	@echo "Available targets:"
	@echo "  build          - Build the binary"
	@echo "  build-debug    - Build with debug symbols"
	@echo "  deps           - Download dependencies"
	@echo "  test           - Run tests"
	@echo "  test-coverage  - Run tests with coverage"
	@echo "  lint           - Run linter"
	@echo "  clean          - Clean build artifacts"
	@echo "  install        - Install to /usr/local/bin"
	@echo "  uninstall      - Remove from /usr/local/bin"
	@echo "  setup-dirs     - Create cache/data directories"
	@echo "  run            - Build and run locally"
	@echo "  build-all      - Build for linux amd64/arm64"
	@echo "  systemd        - Generate systemd service file"
	@echo "  install-service- Install systemd service"
	@echo "  check-update   - Check for available updates"
	@echo "  update-deps    - Update dependencies"
	@echo "  update         - Update from git and rebuild"
	@echo "  upgrade        - Update and reinstall"
