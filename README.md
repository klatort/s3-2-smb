# S3SMB-Gateway

A FUSE-based filesystem that mounts an S3 bucket as a local folder on Linux, designed to be shared via Samba to Windows clients.

## Features

- **Metadata Caching**: File metadata and Windows ACLs (xattrs) are stored in a local SQLite database, avoiding S3 hits for directory listings
- **Lazy Loading**: File bytes are only downloaded when actually requested
- **16MB Chunking**: Data is downloaded in 16MB chunks, not whole files
- **Upload-on-Close**: Writes are buffered locally and uploaded to S3 when the file is closed
- **Extended Attributes**: Full support for xattrs, enabling Windows ACL preservation via Samba

## Requirements

- Linux with FUSE support
- Go 1.21 or later
- FUSE libraries (`libfuse-dev` on Debian/Ubuntu)

## Installation

```bash
# Install dependencies (Debian/Ubuntu)
sudo apt-get install fuse libfuse-dev

# Build
go build -o s3smb-gateway ./cmd/s3smb-gateway

# Install
sudo cp s3smb-gateway /usr/local/bin/
```

## Usage

### Command Line

```bash
# Basic usage
s3smb-gateway -bucket my-bucket -region us-east-1 -mount /mnt/s3

# With S3-compatible endpoint (MinIO, etc.)
s3smb-gateway -bucket my-bucket -region us-east-1 -endpoint http://localhost:9000 -mount /mnt/s3

# With configuration file
s3smb-gateway -config /etc/s3smb-gateway/config.json

# With initial S3 sync
s3smb-gateway -config /etc/s3smb-gateway/config.json -sync
```

### Configuration File

Create a configuration file (see `config.example.json`):

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

### Credentials (Security Best Practices)

**⚠️ Never store credentials in configuration files!**

Credentials are resolved in this order (most secure first):

1. **Environment variables** (recommended):
   ```bash
   export AWS_ACCESS_KEY_ID="your-access-key"
   export AWS_SECRET_ACCESS_KEY="your-secret-key"
   # Or use S3_ prefix for non-AWS services:
   export S3_ACCESS_KEY="your-access-key"
   export S3_SECRET_KEY="your-secret-key"
   ```

2. **AWS Profile** (recommended for development):
   ```bash
   # In config.json:
   # "profile": "my-profile"
   # Or via environment:
   export AWS_PROFILE=my-profile
   ```
   
   Profiles are stored in `~/.aws/credentials`:
   ```ini
   [my-profile]
   aws_access_key_id = YOUR_KEY
   aws_secret_access_key = YOUR_SECRET
   ```

3. **AWS shared credentials file** (`~/.aws/credentials`)

4. **IAM instance role** (for EC2/ECS - most secure for cloud deployments)

#### Environment Variable Reference

| Variable | Description |
|----------|-------------|
| `AWS_ACCESS_KEY_ID` / `S3_ACCESS_KEY` | Access key ID |
| `AWS_SECRET_ACCESS_KEY` / `S3_SECRET_KEY` | Secret access key |
| `AWS_PROFILE` / `S3_PROFILE` | AWS profile name |
| `AWS_REGION` / `S3_REGION` | Region (overrides config) |
| `S3_BUCKET` | Bucket name (overrides config) |
| `S3_ENDPOINT` | Endpoint URL (overrides config) |

#### Using with systemd

For systemd services, use a separate environment file:

```bash
# /etc/s3smb-gateway/credentials (chmod 600)
AWS_ACCESS_KEY_ID=your-key
AWS_SECRET_ACCESS_KEY=your-secret
```

```ini
# In systemd service file
[Service]
EnvironmentFile=/etc/s3smb-gateway/credentials
```

## Samba Integration

To share the mounted S3 bucket via Samba:

1. Mount the filesystem:
```bash
s3smb-gateway -config /etc/s3smb-gateway/config.json &
```

2. Configure Samba (`/etc/samba/smb.conf`):
```ini
[s3share]
    path = /mnt/s3
    browseable = yes
    read only = no
    guest ok = no
    valid users = @smbusers
    
    # Enable extended attributes for Windows ACLs
    ea support = yes
    store dos attributes = yes
    map acl inherit = yes
    
    # VFS objects for ACL support
    vfs objects = acl_xattr
    acl_xattr:ignore system acls = yes
```

3. Restart Samba:
```bash
sudo systemctl restart smbd
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Windows Clients                          │
│                    (via Samba/SMB)                          │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                    Samba Server                             │
│              (smbd with acl_xattr VFS)                      │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                  S3SMB-Gateway (FUSE)                       │
│                                                             │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐  │
│  │   SQLite    │  │   Chunk     │  │     Upload          │  │
│  │  Metadata   │  │   Manager   │  │     Manager         │  │
│  │   Cache     │  │  (16MB)     │  │  (upload-on-close)  │  │
│  └─────────────┘  └─────────────┘  └─────────────────────┘  │
│         │                │                    │             │
│         └────────────────┼────────────────────┘             │
│                          │                                  │
└──────────────────────────┼──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                    AWS S3 / S3-Compatible                   │
│                    (MinIO, Ceph, etc.)                      │
└─────────────────────────────────────────────────────────────┘
```

## Key Components

### SQLite Metadata Cache (`db/`)
- Stores file/directory metadata locally
- Caches Windows ACLs as extended attributes
- Tracks chunk download status
- Manages pending uploads queue

### Chunk Manager (`chunks/`)
- Downloads files in 16MB chunks on demand
- LRU cache eviction when cache limit reached
- Prefetching for sequential reads
- Integrity verification via SHA256

### Upload Manager (`uploader/`)
- Buffers writes to local temp files
- Uploads to S3 on file close
- Automatic retry with exponential backoff
- Handles crash recovery (pending uploads)

### FUSE Filesystem (`fuse/`)
- Implements bazil.org/fuse interfaces
- Maps S3 objects to POSIX filesystem
- Full xattr support for ACLs
- Async S3 operations where possible

## Performance Tuning

### Chunk Size
Default is 16MB. Adjust based on your workload:
- Larger chunks = fewer requests, higher latency for small reads
- Smaller chunks = more requests, lower memory usage

### Cache Size
Set `max_cache_size` to limit disk usage for cached chunks:
- 0 = unlimited
- Recommended: 2-10GB depending on available disk space

### Read-ahead
The FUSE mount uses async read with readahead equal to chunk size. This optimizes sequential reads.

## Limitations

- **Eventual Consistency**: Changes may not be immediately visible in S3 or from other clients
- **No Directory ETags**: Directories don't have ETags in S3, so directory freshness relies on explicit sync
- **Single Writer**: Concurrent writes to the same file from multiple clients may cause conflicts
- **Linux Only**: FUSE is Linux-specific (WSL2 support possible but untested)

## Troubleshooting

### Mount fails with "permission denied"
- Ensure `user_allow_other` is set in `/etc/fuse.conf`
- Run with `sudo` or add user to `fuse` group

### Slow directory listings
- Run with `-sync` flag to populate metadata cache
- Consider increasing SQLite cache size

### Upload failures
- Check S3 credentials and permissions
- Look for pending uploads in database
- Increase retry count in configuration

## License

MIT License
