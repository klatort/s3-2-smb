#!/bin/bash

# remove_mount.sh - Automated Cleanup Script for S3SMB Gateway
# Usage: ./remove_mount.sh <bucket-name> [--keep-data <flags>]

set -e

if [ -z "$1" ]; then
    echo "Usage: $0 <bucket-name> [--keep-data <flags>]"
    echo "Flags for --keep-data:"
    echo "  j : Keep JSON config"
    echo "  e : Keep ENV credentials"
    echo "  s : Keep SQL caching/db"
    echo "Example: $0 mybucket --keep-data jes"
    exit 1
fi

MOUNT_NAME=$1
shift

SHARE_NAME="${MOUNT_NAME}"
KEEP_FLAGS=""

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --share-name) SHARE_NAME="$2"; shift ;;
        --keep-data) KEEP_FLAGS="$2"; shift ;;
        *) echo "Unknown parameter passed: $1"; exit 1 ;;
    esac
    shift
done

JSON_CONFIG="/etc/s3smb-gateway/${MOUNT_NAME}.json"
SMB_CONF="/etc/samba/smb.conf"
MOUNT_PATH="/mnt/s3/${MOUNT_NAME}"

echo "Uninstalling S3SMB Mount: ${MOUNT_NAME}..."

# 1. Stop and Disable Systemd instance natively
echo "Stopping s3smb-gateway@${MOUNT_NAME}..."
systemctl stop "s3smb-gateway@${MOUNT_NAME}" || echo "Service already stopped or does not exist."
systemctl disable "s3smb-gateway@${MOUNT_NAME}" || echo "Service already disabled."

# Ensure mount point and any stale fuse attachments are deleted
echo "Cleaning up FUSE mount point at ${MOUNT_PATH}..."
if mountpoint -q "$MOUNT_PATH" 2>/dev/null || grep -q "$MOUNT_PATH" /proc/mounts; then
    umount -l "$MOUNT_PATH" 2>/dev/null || fusermount -u -z "$MOUNT_PATH" 2>/dev/null || true
fi
if [ -d "$MOUNT_PATH" ]; then
    rmdir "$MOUNT_PATH" 2>/dev/null || true
fi

# 2. Revert the Samba Block
echo "Cleaning Samba configuration..."
if grep -q "^\[${SHARE_NAME}\]" "$SMB_CONF"; then
    # We will use awk to smoothly decouple the specific block section
    # A simple but highly fault-tolerant backup mechanism first:
    cp "$SMB_CONF" "${SMB_CONF}.backup-$(date +%s)"
    
    # Delete the block proactively and dynamically (AWK stops skipping when the next [Share] tag appears)
    awk -v share="[${SHARE_NAME}]" '
        /^\[.*\]/ {
            if ($0 == share) { skip = 1 }
            else { skip = 0 }
        }
        !skip { print }
    ' "$SMB_CONF" | cat -s > "${SMB_CONF}.tmp" && mv "${SMB_CONF}.tmp" "$SMB_CONF"
    
    echo "Successfully erased Samba block."
else
    echo "Samba block [${SHARE_NAME}] was not found. Skipping."
fi

# 3. Reload Samba
echo "Restarting Samba daemon to sever active associations..."
systemctl restart smbd

# 4. Storage & Cache Cleanup
KEEP_JSON=false
KEEP_ENV=false
KEEP_SQL=false

if [[ "$KEEP_FLAGS" == *"j"* ]]; then KEEP_JSON=true; fi
if [[ "$KEEP_FLAGS" == *"e"* ]]; then KEEP_ENV=true; fi
if [[ "$KEEP_FLAGS" == *"s"* ]]; then KEEP_SQL=true; fi

echo "Evaluating Storage & Cache Cleanup..."

if [ "$KEEP_JSON" = false ]; then
    echo "Purging JSON configuration..."
    rm -f "${JSON_CONFIG}"
else
    echo "Retaining JSON configuration (--keep-data j)."
fi

if [ "$KEEP_ENV" = false ]; then
    echo "Purging ENV credentials..."
    rm -f "/etc/s3smb-gateway/${MOUNT_NAME}.env"
else
    echo "Retaining ENV credentials (--keep-data e)."
fi

if [ "$KEEP_SQL" = false ]; then
    echo "Purging SQLite caches and database..."
    rm -rf "/var/cache/s3smb-gateway/${MOUNT_NAME}" "/var/lib/s3smb-gateway/${MOUNT_NAME}"
else
    echo "Retaining SQLite caches and database (--keep-data s)."
fi
echo "--------------------------------------------------------"
echo "Cleanup Complete! The corrupted or old mount has been completely neutralized."
