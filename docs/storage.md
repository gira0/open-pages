# Site storage

open-pages keeps the files of deployed sites in a **storage backend**. The database
(`data.db`) always stays on the local volume; only site files move.

| `[storage] type` | Where site files live |
|---|---|
| `local` (default) | `<datapath>/op_data/<site>/versions/<version>/` plus a `current` symlink |
| `s3` | Objects in an S3-compatible bucket |

Both backends keep every version of a site (`keep_versions`), switch versions atomically
(deploy, rollback), and serve through the same code, so quotas, previews and audit behave the
same. Uploads are always extracted on **local disk** first (under `tmppath`), because the
extractor needs a real file system and enforces its limits there. With `s3` the volume
therefore only has to hold the database and that scratch space.

## S3

```ini
[storage]
type = s3
s3_endpoint = https://s3.corp:9000   ; empty means Amazon S3
s3_region = us-east-1                ; default; many compatible servers accept any value
s3_bucket = open-pages               ; must exist, open-pages does not create it
s3_prefix = prod                     ; optional, lets several instances share a bucket
s3_path_style = true                 ; <endpoint>/<bucket>/..., needed by most self-hosted servers
s3_access_key = ...
s3_secret_key = ...
```

Keep the keys out of the file: `OPEN_PAGES_STORAGE_S3_ACCESS_KEY`,
`OPEN_PAGES_STORAGE_S3_SECRET_KEY` and their `..._FILE` forms work like every other setting.
With both keys empty the AWS default credential chain is used (environment, shared
credentials, IAM role, web identity), which suits Amazon S3 and EKS.

The service account needs `s3:GetObject`, `PutObject`, `DeleteObject` and `ListBucket` on the
bucket (and prefix). Nothing else: open-pages never creates buckets, sets ACLs or reads
policies.

### Helm chart

Set `config.storage.type: s3` and the `config.storage.s3.*` values; see
[Kubernetes](kubernetes.md#s3-site-storage). The keys come from the chart's Secret like the
other secrets and reach the app as `OPEN_PAGES_STORAGE_S3_*_FILE`.

### Compatible servers

open-pages uses only the plain object API (PUT, GET with `Range`, HEAD, LIST, multi-object
DELETE), no vendor extensions, so any server that implements it works: Amazon S3, Ceph RGW,
SeaweedFS, Garage, and others. CI runs the storage conformance suite against
[Garage](https://garagehq.deuxfleurs.fr/) (AGPL-3.0, developed in France) and against an
in-process fake. **MinIO is not recommended**: its community edition no longer ships maintained images or
binaries.

### Layout and consistency

```
<prefix>/sites/<site>/<version>/<file>       the files of one version
<prefix>/sites/<site>/_versions/<version>    marker, written last, body = size in bytes
<prefix>/sites/<site>/.current               the live version id
```

- A version exists once its marker does, so an interrupted deploy leaves no half-visible
  version; the stray objects are removed on the failure path.
- Switching or rolling back is a single `PUT` of `.current`. S3 offers read-after-write
  consistency for new objects and overwrites, so the next request sees the new version.
  A server without that guarantee can serve the old version briefly.
- Deleting a version removes its marker first, deleting a site removes `.current` first, so
  readers stop seeing it before the objects go.
- Each request makes a few small calls (read `.current`, `HEAD` the file, `GET`). Responses
  carry `Cache-Control: no-cache` plus an `ETag`, so browsers revalidate cheaply, but a busy
  site benefits from a caching reverse proxy in front.
- Symbolic links in an archive are never uploaded.

### Limits

- Uploading a version runs under the same lock as every other deploy, rollback and delete.
  Each call has a deadline (30 seconds for a marker or pointer, 5 minutes to delete a
  version or site, 15 minutes to upload one version) and the HTTP client gives up when a
  response does not start within 60 seconds. A slow or stuck object store therefore fails the
  deploy with an error instead of freezing the others, but a deploy of a large site over a
  slow link can hit the 15 minutes.
- A crash in the middle of a deploy can leave uploaded objects without a marker. They are
  never served, but they take space until someone removes the version's prefix, or the
  site is deleted. An `http://` endpoint sends the keys and the site contents unencrypted;
  open-pages logs a warning for any such endpoint that is not on a loopback address.

- One open-pages instance writes to a prefix. Running several writers is not supported (the
  deploy lock is in process, as is the database).
- Moving existing sites between backends is not automated: redeploy them.
