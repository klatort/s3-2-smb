---
applyTo: '**'
---
I am building a custom FUSE-based file system in Go called "S3SMB-Gateway". Goal: Mount an S3 bucket as a local folder on Linux, which will then be shared via Samba to Windows clients. Key Constraints:

    Metadata Caching: Do not hit S3 for directory listings. Store file metadata and Windows ACLs (xattrs) in a local SQLite database.

    Lazy Loading: Only download actual file bytes when requested.

    Chunking: Download data in 16MB chunks, not whole files.

    Safety: "Upload-on-Close" strategy. Writes are local first, then uploaded to S3 on file close.

    Libraries: Use bazil.org/fuse, gorm.io/gorm (SQLite), and github.com/aws/aws-sdk-go-v2.