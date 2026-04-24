# S3SMB-Gateway

A FUSE-based filesystem that mounts an S3 bucket as a local folder on Linux, designed to be shared via Samba to Windows clients dynamically.

## Architecture & Features

- **Asynchronous SQLite Metadata Sync**: File metadata and Windows ACLs (xattrs) are stored locally in a highly concurrent SQLite `WAL` database. Directory listings are served instantly from the database without invoking standard rate limits from AWS API `ListObjects` queries.
- **Background Provisioning**: The Gateway natively syncs the S3 bucket lazily in the background while the OS mounts the filesystem immediately at boot.
- **Lazy Zero-copy Staging**: Files are only resolved and pulled down natively strictly when they are forcefully requested or opened for `Write` operations, bypassing expensive eager-fetches.
- **Extended Attributes**: Native xattr support inherently enables Windows ACL preservation natively through Samba.

## Requirements

- Linux with FUSE support
- Go 1.21 or later
- FUSE libraries (`sudo apt-get install fuse libfuse-dev` on Debian/Ubuntu)

## Deployment (Simplified)

Using the included Makefile, deployment and daemon management is fully automated. This will build the application, distribute configurations, configure systemd, and prepare the core orchestration logic:

```bash
# Install dependencies, compile core binaries, and orchestrate the unit templates
make install

# To seamlessly force update binaries from Git and gracefully redeploy
make update

# To instantly halt all deployed mounts globally and erase the tools entirely
make uninstall
```

## Configuration & Mounting Multiple Buckets

The deployment uses a **systemd template unit** (`s3smb-gateway@.service`), which dynamically isolates caches, databases, and mount points precisely based on the configuration name. This allows you to run an unlimited number of buckets completely independently.

When installed, a default config file is mapped to `/etc/s3smb-gateway/default.json`. You can create as many config domains as you need (e.g., `bucketA.json`, `bucketB.json`). Do not worry about mapping exact sub-folders manually—the systemd template naturally orchestrates isolated databases strictly per mount!

```json
{
  "s3": {
    "bucket": "my-bucket",
    "region": "us-east-1",
    "endpoint": "",
    "profile": "",
    "prefix": ""
  },
  "chunk_size": 16777216,
  "max_cache_size": 10737418240,
  "debug": false
}
```

> **Note on Credentials:** The gateway uses standard AWS credential chains natively. While you can use fallback profiles, it explicitly binds credentials safely deployed using standalone `.env` containers via the custom automation scripts rather than trusting system daemon scopes completely blind.

## 🚀 Automated Mount Provisioning (Recommended)

To completely eliminate the danger of hand-typing JSON arrays or creating configuration datasets natively incorrectly, simply utilize the built-in orchestrator!

Execute the deployment script targeting your specific mount name and optional AWS routing flags:

```bash
chmod +x ./add_mount.sh

# Simple Deployment (Assumes the S3 bucket is also named "mybucket")
sudo ./add_mount.sh mybucket

# Advanced Deployment using decoupled target buckets, Sub-folders and Custom Endpoints
sudo ./add_mount.sh marketing_share \
   --share-name "marketing share public" \
   --bucket "obs-marketing-prod-2024" \
   --region cn-north-4 \
   --endpoint "obs.cn-north-4.myhuaweicloud.com" \
   --access-key "YOUR_ACCESS_KEY" \
   --secret-key "YOUR_SECRET_KEY" \
   --prefix "media/videos/"
```

The script will automatically securely generate `/etc/s3smb-gateway/mybucket.json` for you, inject `[mybucket]` directly into your `/etc/samba/smb.conf`, configure your FUSE systemd template natively, and start the cache engine instantly without dropping connections to your other running S3 shares!

### 🧹 Automated Mount Cleanup

To safely orchestrate the absolute removal of a compromised, obsolete, or poorly configured mount:

```bash
chmod +x ./remove_mount.sh
sudo ./remove_mount.sh badbucket

# Or if you used a custom decoupled Share Name:
sudo ./remove_mount.sh marketing_share --share-name "marketing share public"
```

This ensures the Samba node logic, active hooks, and daemon binds are all definitively neutralized safely.

### Manual Integration

If you prefer to configure manually or advanced ACL tuning is required, you can create the Samba entries in `/etc/samba/smb.conf` manually:

```ini
[mybucket]
    path = /mnt/s3/mybucket
    browseable = yes
    read only = no
    guest ok = yes
```

Then start the gateway manually:
```bash
sudo systemctl enable --now s3smb-gateway@mybucket
sudo systemctl restart smbd
```

## Troubleshooting

- **Service Status:** Check gateway health using `systemctl status s3smb-gateway` or view logs with `journalctl -u s3smb-gateway -f`.
- **Permission Denied limits:** Ensure `user_allow_other` is enabled in `/etc/fuse.conf`.

## License

MIT License
