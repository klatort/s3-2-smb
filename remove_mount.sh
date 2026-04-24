#!/bin/bash

# remove_mount.sh - Automated Cleanup Script for S3SMB Gateway
# Usage: ./remove_mount.sh <bucket-name>

set -e

if [ -z "$1" ]; then
    echo "Usage: $0 <bucket-name>"
    echo "Example: $0 mybucket"
    exit 1
fi

MOUNT_NAME=$1
shift

SHARE_NAME="${MOUNT_NAME}"

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --share-name) SHARE_NAME="$2"; shift ;;
        *) echo "Unknown parameter passed: $1"; exit 1 ;;
    esac
    shift
done

JSON_CONFIG="/etc/s3smb-gateway/${MOUNT_NAME}.json"
SMB_CONF="/etc/samba/smb.conf"

echo "Uninstalling S3SMB Mount: ${MOUNT_NAME}..."

# 1. Stop and Disable Systemd instance natively
echo "Stopping s3smb-gateway@${MOUNT_NAME}..."
systemctl stop "s3smb-gateway@${MOUNT_NAME}" || echo "Service already stopped or does not exist."
systemctl disable "s3smb-gateway@${MOUNT_NAME}" || echo "Service already disabled."

# 2. Revert the Samba Block
echo "Cleaning Samba configuration..."
if grep -q "^\[${SHARE_NAME}\]" "$SMB_CONF"; then
    # We will use sed to delete the block from [SHARE_NAME] to the next blank line or next bracket
    # A simple but highly fault-tolerant backup mechanism first:
    cp "$SMB_CONF" "${SMB_CONF}.backup-$(date +%s)"
    
    # Delete the block dynamically exactly as created by add_mount.sh
    sed -i "/^\[${SHARE_NAME}\]/,/^$/d" "$SMB_CONF"
    echo "Successfully erased Samba block."
else
    echo "Samba block [${SHARE_NAME}] was not found. Skipping."
fi

# 3. Reload Samba
echo "Restarting Samba daemon to sever active associations..."
systemctl restart smbd

# 4. Optional Storage Cleanup
echo "The configuration files have been retained for your safety."
echo "If you wish to fully delete the configurations and its sqlite caches entirely, run:"
echo "sudo rm ${JSON_CONFIG} /etc/s3smb-gateway/${MOUNT_NAME}.env && sudo rm -rf /var/cache/s3smb-gateway/${MOUNT_NAME} /var/lib/s3smb-gateway/${MOUNT_NAME}"
echo "--------------------------------------------------------"
echo "Cleanup Complete! The corrupted or old mount has been completely neutralized."
