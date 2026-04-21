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

Using the included Makefile, deployment and daemon management is fully automated. This will build the application, distribute configurations, configure systemd, and start the background service immediately:

```bash
# Install dependencies, compile, setup systemd, and start the service
make install

# To later update from Git and gracefully redeploy
make update

# To stop the service and remove the tool completely
make uninstall
```

## Configuration

When installed, a template config file is dropped at `/etc/s3smb-gateway/config.json`. Update it with your S3 credentials and restart the service:

```json
{
  "s3": {
    "bucket": "my-bucket",
    "region": "us-east-1",
    "endpoint": "",
    "profile": "",
    "prefix": ""
  },
  "mount_point": "/mnt/s3",
  "cache_dir": "/var/cache/s3smb-gateway",
  "db_path": "/var/lib/s3smb-gateway/metadata.db",
  "chunk_size": 16777216,
  "max_cache_size": 10737418240,
  "debug": false
}
```

> **Note on Credentials:** The gateway uses standard AWS credential chains. You can set credentials via environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`), standard AWS profiles, or EC2/ECS IAM instance profiles.

Restart the gateway after editing your configuration:

```bash
sudo systemctl restart s3smb-gateway
```

## Samba Integration

To share the actively mounted S3 bucket (`/mnt/s3`) with your Windows network:

1. Configure Samba (`/etc/samba/smb.conf`):

```ini
[global]
    stat cache = no

[s3share]
    path = /mnt/s3
    browseable = yes
    read only = no
    guest ok = no
    valid users = @smbusers
    
    use sendfile = no
    strict locking = no
    oplocks = no
    level2 oplocks = no
    kernel oplocks = no
    dos filemode = yes

    vfs objects = acl_tdb
    store dos attributes = yes
    map acl inherit = yes
    acl_tdb:ignore system acls = yes
```

1. Reload Samba:

```bash
sudo systemctl restart smbd
```

## Troubleshooting

- **Service Status:** Check gateway health using `systemctl status s3smb-gateway` or view logs with `journalctl -u s3smb-gateway -f`.
- **Permission Denied limits:** Ensure `user_allow_other` is enabled in `/etc/fuse.conf`.

## License

MIT License
