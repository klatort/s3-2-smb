# S3SMB-Gateway

A FUSE-based filesystem that mounts an S3 bucket as a local folder on Linux, designed to be shared via Samba to Windows clients.

## Features

- **Metadata Caching**: File metadata and Windows ACLs (xattrs) are stored locally in SQLite, avoiding S3 API limits for directory listings.
- **Lazy Loading & Chunking**: File bytes are downloaded on-demand in 16MB chunks.
- **Upload-on-Close**: Writes are buffered locally and pushed to S3 when the file is closed.
- **Extended Attributes**: Native xattr support inherently enables Windows ACL preservation via Samba.

## Requirements

- Linux with FUSE support
- Go 1.21 or later
- FUSE libraries (`sudo apt-get install fuse libfuse-dev` on Debian/Ubuntu)

## Deployment (Simplified)

Using the included Makefile, deployment and daemon management is fully automated. This will build the application, distribute configurations, configure systemd, and start the default background service immediately:

```bash
# Install dependencies, compile, setup the systemd template, and start the 'default' service
make install

# To later update from Git and gracefully redeploy
make update

# To stop all services and remove the tool completely
make uninstall
```

## Configuration & Mounting Multiple Buckets

The deployment uses a **systemd template unit** (`s3smb-gateway@.service`), which dynamically provisions isolated caches, databases, and mount points based on the configuration name. This means you can run an unlimited number of buckets natively.

When installed, a default config file is dropped at `/etc/s3smb-gateway/default.json`. You can create as many configurations as you want in `/etc/s3smb-gateway/` (e.g., `bucketA.json`, `bucketB.json`). Do not worry about specifying paths within the configuration—the template automatically injects strict directories!

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

> **Note on Credentials:** The gateway uses standard AWS credential chains. You can set credentials via environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`), standard AWS profiles, or EC2/ECS IAM instance profiles.

## 🚀 Automated Mount Provisioning (Recommended)

To eliminate the hassle of manually adjusting Samba configuration blocks and creating configuration datasets by hand, we've provided a simple deployment automation script!

Just run the deployment script with your target bucket and optional AWS routing flags:

```bash
chmod +x ./add_mount.sh

# Simple Deployment (Generates bucket config natively)
sudo ./add_mount.sh mybucket

# Advanced Deployment using Sub-folders and Custom Endpoints
sudo ./add_mount.sh bucketB --prefix "media/videos/" --region "us-west-2" --endpoint "http://minio.local:9000"
```

The script will automatically securely generate `/etc/s3smb-gateway/mybucket.json` for you, inject `[mybucket]` directly into your `/etc/samba/smb.conf`, configure your FUSE systemd template natively, and start the cache engine instantly without dropping connections to your other running S3 shares!

### 🧹 Automated Mount Cleanup

To safely orchestrate the absolute removal of a compromised, obsolete, or poorly configured mount:

```bash
chmod +x ./remove_mount.sh
sudo ./remove_mount.sh badbucket
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
