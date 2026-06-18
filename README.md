# localfront

**A local Amazon CloudFront emulator, driven by CloudFormation templates.**

localfront reads the same `AWS::CloudFront::*` resources you deploy to AWS — from a hand-written template or `cdk synth` output — and runs a working data plane on your machine: a reverse proxy with cache behaviors, CloudFront Functions, KeyValueStore lookups, and signed URL / signed cookie verification, serving S3 origins from an external S3-compatible object store.

> **Status: Proof of Concept.** This README defines the PoC scope. Behavior, defaults, and file formats may change without notice.

## Why

CloudFront behavior — path-based routing across cache behaviors, CloudFront Functions, signed URLs, error-page fallbacks — can usually only be verified against a real distribution. Deploys take minutes and are awkward to run in CI. localfront provides a faithful-enough local stand-in that starts instantly from the template you already have.

localfront deliberately has **no management API**. Configuration is a CloudFormation template, hot-reloaded on change:

- **CloudFormation users** — point localfront at your template.
- **CDK users** — point it at the synthesized template in `cdk.out/`. Resource types localfront doesn't know are skipped with a warning, so full-app templates load as-is.
- **Terraform users** — a companion converter is planned as a **separate project**: it consumes a Terraform plan (`terraform show -json <planfile>`), so all HCL expressions are already resolved by Terraform itself, and emits a CloudFormation template covering the `aws_cloudfront_*` resources (intra-plan references become `Ref` / `GetAtt`). No general-purpose Terraform→CFN converter exists; restricting scope to CloudFront resources is what makes it tractable.

## PoC scope

### Configuration: CloudFormation template

- Loads one or more templates (JSON / YAML) and **hot-reloads** them on file change.
- Supported resource types: `AWS::CloudFront::Distribution`, `AWS::CloudFront::Function`, `AWS::CloudFront::KeyValueStore`, `AWS::CloudFront::CachePolicy`, `AWS::CloudFront::OriginRequestPolicy`, `AWS::CloudFront::ResponseHeadersPolicy`, `AWS::CloudFront::PublicKey`, `AWS::CloudFront::KeyGroup`, `AWS::CloudFront::OriginAccessControl`. Unknown resource types are skipped with a warning.
- AWS **managed policies** are built in under their well-known IDs (`Managed-CachingOptimized`, `Managed-CachingDisabled`, `Managed-AllViewer`, `Managed-CORS-S3Origin`, …). Legacy `ForwardedValues` is also accepted.
- Intrinsic functions: a pragmatic subset — `Ref` and `Fn::GetAtt` between resources in the loaded templates, `Fn::Sub`, `Fn::Join`, `Fn::FindInMap`, and `Parameters` (defaults, overridable with `--parameter key=value`). Anything unresolvable (e.g. `Fn::ImportValue`) fails template loading with a clear error.
- KeyValueStore contents are seeded from the resource's `ImportSource` (fetched from the configured S3-compatible store) or from a local JSON file via `--kvs-seed <store>=<file.json>`. A `--kvs-seed` replaces the store's contents and skips the `ImportSource` fetch entirely, so it works offline without `--s3-endpoint`.
- localfront is **stateless**: the templates, seed files, and the object store are the entire configuration. There is no state directory and no write API.

### Data plane (the actual proxy)

- **Cache behaviors** — path pattern matching with CloudFront's precedence rules, the default behavior, allowed / cached methods, and per-behavior `Compress` (gzip / brotli).
- **Origins** — custom HTTP(S) origins (origin path, custom headers, protocol policy), and S3 origins backed by an external S3-compatible object store (see [S3 origins](#s3-origins-external-object-storage)).
- **Default root object** and **custom error responses** — including the classic SPA fallback (`403/404 → /index.html` with status 200).
- **CloudFront Functions** at viewer-request / viewer-response, with cloudfront-js 2.0 semantics and KVS bindings.
- **Signed URLs & signed cookies** — canned and custom policies, verified against the trusted key groups of restricted behaviors. A canned signature covers the exact resource URL, so localfront reconstructs it from `--public-host` (required; env `LOCALFRONT_PUBLIC_HOST`) — the host (optionally `host:port`) you signed the URLs for. The port, when present, is part of the signed resource (real CloudFront only serves on 443/80, so its signed URLs never carry one — sign your local URLs for the address you actually reach localfront at). A custom policy whose `Resource` includes a host (e.g. `https://*.tenants.example.com/*`) has that host matched too, against the same `--public-host`; leave `--public-host` empty to match each request's own `Host` instead — the wildcard-subdomain case, where every tenant arrives on a different host.
- **Host-based routing** — distributions are matched by their aliases (CNAMEs); each also gets a stable generated ID derived from its template logical ID, reachable as `<distribution-id>.cloudfront.localhost`.
- **CloudFront request/response headers** — `X-Amz-Cf-Id`, `Via`, `X-Cache`, `X-Forwarded-For`, and the `CloudFront-Viewer-*` / device-detection headers selected by the origin request policy. Viewer header values (country, device, …) can be overridden per request to test geo- or device-dependent code.
- **Pass-through of conditional and range requests** (`If-None-Match` / `If-Modified-Since`, `Range`, `HEAD`) to origins.

### S3 origins (external object storage)

localfront does not embed an object store. S3 origins are served by an **external S3-compatible storage** — [RustFS](https://github.com/rustfs/rustfs) is the reference companion (other S3-compatible stores such as MinIO should work as well). At request time, localfront acts as an **S3 API client**: it resolves the bucket from the origin's domain name and fetches objects with SigV4-signed `GetObject` / `HeadObject` calls against the configured endpoint, mapping S3 errors (`NoSuchKey`, `AccessDenied`, …) to the status codes CloudFront would return — which then feed custom error responses as usual.

- Origin domain names of the form `<bucket>.s3.<region>.amazonaws.com` are resolved to `<s3-endpoint>/<bucket>` (path-style), so templates written for production work unchanged.
- Conditional and range requests are forwarded as S3 request parameters (`Range`, `If-None-Match`, …).
- `AWS::CloudFront::OriginAccessControl` resources are accepted but not enforced — localfront always accesses the store with the credentials given at startup; bucket policies on the store side are not consulted.
- Assets are uploaded directly to the store with the usual tooling (`aws s3 sync --endpoint-url …`); localfront is not involved in uploads.

## Caching

The PoC **does not cache**. Cache policies are still interpreted — they determine the cache key and therefore which headers, cookies, and query strings reach your origin — but every request is forwarded and answered with `X-Cache: Miss from localfront`. This keeps local behavior deterministic in tests. An optional in-memory cache (honoring cache policies and TTLs, with `X-Cache: Hit` and a purge command) is on the roadmap.

## Not supported

**Intentionally out of scope, now and later:**

| Feature | Reason |
| --- | --- |
| **Lambda@Edge** | Deliberate. Emulating Lambda runtimes and replication is a different product; CloudFront Functions + KVS cover most local-development use cases. |
| **Management APIs** | By design. localfront reads template files; it does not implement the CloudFront management API, the CloudFormation API, or the `cloudfront-keyvaluestore` data API. Driving localfront directly from Terraform or AWS SDKs is out of scope — use the template workflow above. |

**Accepted but ignored** — template properties that load cleanly but have no effect:

- Price class, IPv6 / HTTP/2 / HTTP/3 flags
- Viewer certificate (ACM / minimum protocol version) — the PoC serves plain HTTP only
- Viewer protocol policy (never redirects to HTTPS locally)
- Geo restrictions (stored, not enforced)
- Origin Shield
- Logging configuration
- Web ACL (WAF) association
- Tags

**Not implemented** — template loading fails with a clear error:

- Origin group failover (planned, see roadmap)
- Field-level encryption
- Real-time logs
- Continuous deployment / `AWS::CloudFront::ContinuousDeploymentPolicy`
- VPC origins, Anycast static IPs, multi-tenant (SaaS Manager) distributions

## Quick start

> The CLI shown below is the planned interface; defaults may change.

```console
$ docker run -d -p 9000:9000 rustfs/rustfs        # S3-compatible origin storage
$ go install github.com/mackee/localfront/cmd/localfront@latest
$ localfront serve --template ./template.yaml \
    --listen        :8080 \
    --public-host   assets.example.test:8080 \
    --s3-endpoint   http://localhost:9000 \
    --s3-access-key rustfsadmin \
    --s3-secret-key rustfsadmin
data plane   http://localhost:8080
template     ./template.yaml (hot reload)
distribution E... [AssetsDistribution]
  http://assets.example.test -> localhost:8080
```

Repeatable flags: `--template`, `--parameter KEY=VALUE` (overrides template parameter defaults), and `--kvs-seed STORE=FILE` (substitutes a local JSON for a KeyValueStore's `ImportSource`). `--listen` defaults to `:8080`; `--log-level` accepts `debug|info|warn|error`. `--public-host` is required (env: `LOCALFRONT_PUBLIC_HOST`).

`--access-log FILE` (env: `LOCALFRONT_ACCESS_LOG`) writes per-request access logs in CloudFront's Standard log format — 33 tab-separated columns with the `#Version:` / `#Fields:` preamble — so the same ETL that consumes the S3-delivered logs in production also consumes the file localfront produces. Default is `-` (stdout); pass an empty string (`--access-log ''` or `LOCALFRONT_ACCESS_LOG=''`) to disable. The startup banner and operational slog output go to stderr, leaving stdout clean for log consumers.

### Container image (GHCR)

Pre-built images are published to `ghcr.io/mackee/localfront` (linux/amd64 + linux/arm64) on every release tag. The image is `gcr.io/distroless/static-debian12:nonroot`-based — about 16 MB, no shell or package manager. Every flag is also reachable as `LOCALFRONT_*`, so the binary can be fully configured by environment:

```console
$ docker run --rm -p 8080:8080 \
    -v $PWD/template.yaml:/etc/localfront/template.yaml:ro \
    -e LOCALFRONT_TEMPLATE=/etc/localfront/template.yaml \
    -e LOCALFRONT_PUBLIC_HOST=assets.example.test:8080 \
    -e LOCALFRONT_S3_ENDPOINT=http://host.docker.internal:9000 \
    -e LOCALFRONT_S3_ACCESS_KEY=rustfsadmin \
    -e LOCALFRONT_S3_SECRET_KEY=rustfsadmin \
    --tmpfs /tmp \
    ghcr.io/mackee/localfront:latest
```

`/tmp` must be writable: localfront sandboxes each CloudFront Function in its own tempdir under `os.TempDir()`. The image declares `VOLUME /tmp`, so `--tmpfs /tmp` (or a named volume) keeps the rest of the root filesystem read-only-able.

Compose example:

```yaml
services:
  localfront:
    image: ghcr.io/mackee/localfront:latest
    ports: ["8080:8080"]
    environment:
      LOCALFRONT_TEMPLATE: /etc/localfront/template.yaml
      LOCALFRONT_PUBLIC_HOST: assets.example.test:8080
      LOCALFRONT_S3_ENDPOINT: http://rustfs:9000
      LOCALFRONT_S3_ACCESS_KEY: rustfsadmin
      LOCALFRONT_S3_SECRET_KEY: rustfsadmin
    volumes:
      - ./template.yaml:/etc/localfront/template.yaml:ro
      - ./seeds:/etc/localfront/seeds:ro
    tmpfs:
      - /tmp
    depends_on: [rustfs]
  rustfs:
    image: rustfs/rustfs
    ports: ["9000:9000"]
```

For repeatable flags via environment, separate entries with commas: `LOCALFRONT_TEMPLATE=/etc/localfront/a.yaml,/etc/localfront/b.yaml`, `LOCALFRONT_KVS_SEED=storeA=/etc/localfront/seeds/a.json,storeB=/etc/localfront/seeds/b.json`.

### Example template

```yaml
Resources:
  AssetsDistribution:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        DefaultRootObject: index.html
        Aliases:
          - assets.example.test
        Origins:
          - Id: s3
            DomainName: assets.s3.us-east-1.amazonaws.com # served by the external store
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: s3
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6 # Managed-CachingOptimized
        CustomErrorResponses:
          - ErrorCode: 404
            ResponseCode: 200
            ResponsePagePath: /index.html
```

### Upload assets and send a request through the distribution

Uploads go straight to the object store; only viewer requests go through localfront:

```console
$ export AWS_ACCESS_KEY_ID=rustfsadmin AWS_SECRET_ACCESS_KEY=rustfsadmin
$ aws --endpoint-url http://localhost:9000 s3 mb s3://assets
$ aws --endpoint-url http://localhost:9000 s3 sync ./dist s3://assets
$ curl -H 'Host: assets.example.test' http://localhost:8080/
```

### Coming from CDK or Terraform

- **CDK** — synthesize and point localfront at the output: `cdk synth && localfront serve --template cdk.out/MyStack.template.json`. Non-CloudFront resources in the template are skipped with a warning.
- **Terraform** — use the planned companion converter (separate project) to turn a plan into a template, then serve it. Until it exists, write a small template by hand mirroring your `aws_cloudfront_distribution`.

## Compatibility notes

- Template properties follow the current `AWS::CloudFront::*` resource specifications, so production templates load unchanged.
- Distribution IDs are generated deterministically from template logical IDs and are stable across restarts.
- S3 origin fetches are signed with SigV4 using the credentials given at startup.

## Roadmap (post-PoC)

- **Terraform plan → CloudFormation companion converter** (separate repository)
- Optional cache emulation: in-memory cache honoring cache policies and TTLs, `X-Cache: Hit`, and a purge command
- `localfront test-function` — run a CloudFront Function against a synthetic event from the CLI
- Origin group failover
- TLS termination with locally trusted certificates
- Real-time logs (Kinesis-shaped variant of CloudFront's log delivery; Standard logs are already supported via `--access-log`)
- Geo restriction enforcement and viewer profile presets (country / device simulation)
