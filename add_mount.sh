#!/bin/bash

# add_mount.sh - Automated Deployment Script for S3SMB Gateway
# Usage: ./add_mount.sh <bucket-name> [--prefix <prefix>] [--endpoint <url>] [--region <region>]

set -e

if [ -z "$1" ]; then
    echo "Usage: $0 <bucket-name> [--prefix <prefix>] [--endpoint <url>] [--region <region>]"
    echo "Example: $0 mybucket --region us-east-1"
    exit 1
fi

MOUNT_NAME=$1
shift

PREFIX=""
ENDPOINT=""
REGION="us-east-1"
ACCESS_KEY=""
SECRET_KEY=""
BUCKET_NAME="${MOUNT_NAME}"
SHARE_NAME="${MOUNT_NAME}"

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --share-name) SHARE_NAME="$2"; shift ;;
        --bucket) BUCKET_NAME="$2"; shift ;;
        --prefix) PREFIX="$2"; shift ;;
        --endpoint) ENDPOINT="$2"; shift ;;
        --region) REGION="$2"; shift ;;
        --access-key) ACCESS_KEY="$2"; shift ;;
        --secret-key) SECRET_KEY="$2"; shift ;;
        *) echo "Unknown parameter passed: $1"; exit 1 ;;
    esac
    shift
done

JSON_CONFIG="/etc/s3smb-gateway/${MOUNT_NAME}.json"
ENV_FILE="/etc/s3smb-gateway/${MOUNT_NAME}.env"
SMB_CONF="/etc/samba/smb.conf"
MOUNT_PATH="/mnt/s3/${MOUNT_NAME}"

echo "Deploying S3SMB Mount: ${MOUNT_NAME}..."

# 1. Generate configuration file if it doesn't exist
if [ ! -f "$JSON_CONFIG" ]; then
    echo "Configuration file ${JSON_CONFIG} does not exist. Generating natively..."
    mkdir -p /etc/s3smb-gateway
    cat > "$JSON_CONFIG" <<EOF
{
  "s3": {
    "bucket": "${BUCKET_NAME}",
    "region": "${REGION}",
    "endpoint": "${ENDPOINT}",
    "profile": "",
    "prefix": "${PREFIX}"
  },
  "mount_point": "${MOUNT_PATH}",
  "cache_dir": "/var/cache/s3smb-gateway/${MOUNT_NAME}",
  "db_path": "/var/lib/s3smb-gateway/${MOUNT_NAME}/metadata.db",
  "chunk_size": 16777216,
  "max_cache_size": 10737418240,
  "debug": false
}
EOF
    echo "Successfully created configuration at ${JSON_CONFIG}"
else
    echo "Configuration file ${JSON_CONFIG} already exists. Using existing configuration."
fi

# 1.5 Generate Environment file securely
if [ -n "$ACCESS_KEY" ] || [ -n "$SECRET_KEY" ]; then
    echo "Securing AWS credentials natively into ${ENV_FILE}..."
    cat > "$ENV_FILE" <<EOF
AWS_ACCESS_KEY_ID=${ACCESS_KEY}
AWS_SECRET_ACCESS_KEY=${SECRET_KEY}
EOF
    chmod 600 "$ENV_FILE"
else
    echo "No explicit credentials provided. Gateway will attempt to use fallback System/EC2 profiles."
fi

# 2. Inject Samba configuration if not present
echo "Configuring Samba share [${SHARE_NAME}]..."
if ! grep -q "^\[${SHARE_NAME}\]" "$SMB_CONF"; then
    cat >> "$SMB_CONF" <<SMBEOF

[${SHARE_NAME}]
   path = ${MOUNT_PATH}
   read only = no
   guest ok = yes
   force user = root

   # VFS modules for Windows compatibility
   vfs objects = catia fruit streams_xattr
   ea support = yes
   store dos attributes = yes
   map archive = no
   map hidden = no
   map system = no

   # CRITICAL: FUSE doesn't support kernel-level locks or oplocks.
   # Without these, Samba tries to use fcntl/POSIX locks that FUSE
   # can't enforce, causing clients to cache stale data indefinitely.
   kernel oplocks = no
   kernel share modes = no
   posix locking = no
   strict locking = no
   smb2 leases = no
SMBEOF
    echo "Successfully injected Samba configuration."
else
    echo "Samba block [${SHARE_NAME}] already exists. Skipping injection."
fi

# 3. Reload Systemd and Enable the specific instance
echo "Reloading systemd daemons..."
systemctl daemon-reload

echo "Enabling and Starting s3smb-gateway@${MOUNT_NAME}..."
systemctl enable --now "s3smb-gateway@${MOUNT_NAME}"

# 4. Reload Samba
echo "Restarting Samba daemon..."
systemctl restart smbd

echo "--------------------------------------------------------"
echo "Deployment Complete!"
echo "Your new S3 bucket placeholder metadata is syncing natively."
echo "You can access the share via: \\\\<your-server-ip>\\${MOUNT_NAME}"
