# ExternalDNS Webhook Provider for NetBird

This project is a webhook provider for [ExternalDNS](https://github.com/kubernetes-sigs/external-dns). It manages records in [NetBird](https://netbird.io) custom DNS zones. ExternalDNS keeps DNS in sync with your Kubernetes Ingresses, Services, and Gateway routes. This provider writes those records to NetBird through the management API. Then NetBird sends them to the peers in the distribution groups of each zone.

## Requirements

| Component   | Version                                         |
| ----------- | ----------------------------------------------- |
| NetBird     | v0.63.0 or later (first release with DNS zones) |
| ExternalDNS | v0.23.0 (tested)                                |

The provider works with NetBird Cloud and with self-hosted management servers.

## How it works

The provider runs as a sidecar, a second container in the ExternalDNS pod. It speaks the ExternalDNS webhook protocol on one side and calls the NetBird management API (`/api/dns/zones/...`) on the other.

Create your zones and their distribution groups in NetBird first, because the provider does not create zones. It manages every zone that its token can see, or only the zones listed in `NETBIRD_ZONES`. Hostnames outside those zones never reach the ExternalDNS plan, and any `--domain-filter` you set applies on top. Each sync looks up the zones again, so a new zone needs no restart.

Reconciliation works per name and type. Each sync reads all zones in one request and makes the smallest set of record writes. Unchanged records stay as they are, and changed targets and TTLs change in place. Only leftover records get a create or a delete. Fewer writes matter, because each write makes NetBird send a network map update to the peers of the zone.

New records go into the most specific zone. With the zones `example.com` and `dev.example.com`, for example, `app.dev.example.com` goes into `dev.example.com`. If `dev.example.com` is not managed, the provider skips the name. A record in `example.com` cannot help, because `dev.example.com` hides it.

One failed record does not stop the rest of the batch. Each change gets its own attempt, and ExternalDNS retries the failures on the next sync.

## Ownership

A registry is the place where ExternalDNS keeps a record of the DNS records that it owns. The default TXT registry keeps this ownership in TXT records. NetBird custom zones hold only A, AAAA, and CNAME records, so you cannot use the TXT registry. The webhook rejects each batch that contains TXT records or other types that it does not support. Use one of the two registries below.

### No ownership: `--registry=noop`

With the noop registry, ExternalDNS treats every A, AAAA, and CNAME record in the managed zones as its own. Under `policy: sync`, it deletes every record that has no source, including records you created by hand in the NetBird dashboard. Two instances that share a zone delete each other's records too. To prevent this, give ExternalDNS zones of its own with `NETBIRD_ZONES` or `domainFilters`, or switch to `policy: upsert-only`, which never deletes.

### Ownership in the cluster: `--registry=crd`

The CRD registry stores a `DNSRecord` resource in Kubernetes for each record that ExternalDNS applied. Only those records enter the plan, so records under other names stay untouched. The registry is alpha upstream, and the ExternalDNS project does not recommend it for production yet. It trusts its own resources instead of reading NetBird, which has these consequences:

- Ownership covers whole names and types. A hand-made record with the same name and type as a managed one gets taken over: repointed at the managed targets, or deleted along with the source.
- Nothing reconciles edits that you make in the NetBird dashboard. A record you delete there stays deleted until its source changes.
- A hand-made record can conflict with a wanted one, such as an A record where ExternalDNS wants a CNAME. That record then fails on every sync until one of the two goes away. The rest of the batch still applies, but the registry marks the whole batch as failed. Records from that batch count as owned only once a later batch succeeds. If their source disappears first, they stay in NetBird.
- If two instances share a zone, they must manage different names. The registry names each `DNSRecord` object after the record, not the owner. Two instances that manage the same name and type in one namespace therefore overwrite each other's ownership.

## Limitations

NetBird allows one CNAME per name, and a CNAME cannot share its name with an A or AAAA record. If a source gives several CNAME targets for one name, the provider uses the first target. It drops the other targets and logs a warning.

The default TTL is 300 seconds. NetBird gives the TTL of a record to the peers as it is, and a TTL of 0 stops caching. If ExternalDNS gives no TTL, the webhook writes 300. To use a different TTL for a record, set the `external-dns.alpha.kubernetes.io/ttl` annotation.

## Quick start

The access token needs read and write access to DNS. The Admin role has this access. If your plan includes the Network Admin role, you can use it instead, because it also has this access.

1. In the NetBird dashboard, go to Team → Service Users.
2. Create a service user.
3. Create an access token for the service user.
4. Store the token in a Kubernetes Secret:

   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: netbird-dns-secret
   stringData:
     NETBIRD_API_TOKEN: <your-access-token>
   ```

5. Add the ExternalDNS chart repository:

   ```sh
   helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
   ```

6. Create a values file with the name `external-dns-netbird-values.yaml`:

   ```yaml
   fullnameOverride: external-dns-netbird
   provider:
     name: webhook
     webhook:
       image:
         repository: ghcr.io/futo-org/external-dns-netbird-webhook
         tag: main # replace with a versioned release tag
       env:
         - name: NETBIRD_API_URL
           value: https://api.netbird.io # or your self-hosted management URL
         - name: NETBIRD_API_TOKEN
           valueFrom:
             secretKeyRef:
               name: netbird-dns-secret
               key: NETBIRD_API_TOKEN
       livenessProbe:
         httpGet:
           path: /healthz
           port: http-webhook
         initialDelaySeconds: 10
         timeoutSeconds: 5
       readinessProbe:
         httpGet:
           path: /readyz
           port: http-webhook
         initialDelaySeconds: 10
         timeoutSeconds: 5
   registry: noop # or crd, see Ownership
   policy: sync # or upsert-only to never delete records
   sources:
     - gateway-httproute
     - service
   domainFilters:
     - internal.example.com # replace with your NetBird zone(s)
   serviceMonitor:
     enabled: true
   ```

7. Install the chart:

   ```sh
   helm install external-dns-netbird external-dns/external-dns \
     -f external-dns-netbird-values.yaml -n external-dns
   ```

For more options, see the [chart values](https://github.com/kubernetes-sigs/external-dns/blob/master/charts/external-dns/values.yaml).

To keep ownership with the [CRD registry](#ownership) instead, replace `registry: noop` with these lines. The chart then installs the `DNSRecord` CRD and the RBAC that it needs.

```yaml
registry: crd
txtOwnerId: netbird # unique per ExternalDNS instance sharing a zone
```

## Configuration

### NetBird connection

| Variable                      | Description                                                                               | Default                  |
| ----------------------------- | ----------------------------------------------------------------------------------------- | ------------------------ |
| `NETBIRD_API_URL`             | Base URL of the management server, without the `/api` suffix.                             | `https://api.netbird.io` |
| `NETBIRD_API_TOKEN`           | Access token for authentication. Required.                                                | -                        |
| `NETBIRD_ZONES`               | Comma-separated domains of the zones to manage. If unset, the webhook manages every zone. | -                        |
| `NETBIRD_APPLY_WORKERS`       | Maximum number of record writes at the same time during a sync (1-20).                    | `5`                      |
| `NETBIRD_RETRY_ATTEMPTS`      | Total attempts for each request, including the first attempt (1-10).                      | `3`                      |
| `NETBIRD_RETRY_INITIAL_DELAY` | Backoff before the first retry.                                                           | `500ms`                  |
| `NETBIRD_RETRY_MAX_DELAY`     | Maximum backoff between retries. It also limits `Retry-After`.                            | `10s`                    |
| `NETBIRD_REQUEST_TIMEOUT`     | Timeout for each attempt of a management API request. If unset, there is no timeout.      | -                        |
| `NETBIRD_APPLY_TIMEOUT`       | Time limit to apply one batch of changes (see [Time limits](#time-limits)).               | `12s`                    |

If your self-hosted management server uses a certificate from a private CA, mount the CA bundle into the container. Then set `SSL_CERT_FILE` to the path of the bundle.

#### Retries

The webhook retries a `429 Too Many Requests` response on every request and honors `Retry-After`. Reads, updates, and deletes also retry on network errors and `5xx` responses. Creates do not, because NetBird can commit a create before the failure, so the next ExternalDNS sync retries them instead. A create that finds an identical record counts as a success, and so does a delete that finds nothing.

#### Time limits

ExternalDNS gives each webhook call a fixed time limit: `--webhook-provider-read-timeout` plus `--webhook-provider-write-timeout` (5s plus 10s by default). A call that runs past the limit makes ExternalDNS exit with a fatal error. An HTTP 5xx answer only shows up in its log, and the next sync tries again.

`NETBIRD_APPLY_TIMEOUT` limits the time to apply one batch of changes. The limit covers the zone lookup and every create, update, and delete. If the time runs out, the webhook answers `500`. The writes that finished stay, and the next sync applies the rest. Keep `NETBIRD_APPLY_TIMEOUT` below the ExternalDNS time limit. If large batches against a slow management server time out again and again, increase both values together.

To read the records, the webhook sends one request. To limit this request, set `NETBIRD_REQUEST_TIMEOUT`. Make sure that `NETBIRD_RETRY_ATTEMPTS × NETBIRD_REQUEST_TIMEOUT`, plus the backoff between attempts, stays below the ExternalDNS time limit. With the default values, `4s` gives a worst case of about 14.3s. But a `Retry-After` header can make each wait between attempts as long as `NETBIRD_RETRY_MAX_DELAY`. Then the worst case is `NETBIRD_RETRY_ATTEMPTS × NETBIRD_REQUEST_TIMEOUT + (NETBIRD_RETRY_ATTEMPTS - 1) × NETBIRD_RETRY_MAX_DELAY`, which is 32s with the default values and `4s`. To stay below the time limit, decrease `NETBIRD_RETRY_MAX_DELAY` (`1s` gives 14s) or increase the ExternalDNS timeouts. If you increase the ExternalDNS timeouts, keep their sum below `SERVER_WRITE_TIMEOUT`.

### Webhook server

| Variable                     | Description                                                      | Default           |
| ---------------------------- | ---------------------------------------------------------------- | ----------------- |
| `SERVER_HOST`                | Bind address of the webhook server.                              | `localhost`       |
| `SERVER_PORT`                | Port of the webhook server.                                      | `8888`            |
| `SERVER_READ_TIMEOUT`        | Timeout to read a request.                                       | `60s`             |
| `SERVER_READ_HEADER_TIMEOUT` | Timeout to read the request headers. It stops slow clients.      | `5s`              |
| `SERVER_WRITE_TIMEOUT`       | Timeout to write a response.                                     | `60s`             |
| `SERVER_IDLE_TIMEOUT`        | Idle timeout for keep-alive connections.                         | `120s`            |
| `SERVER_MAX_HEADER_BYTES`    | Maximum size of the request headers.                             | `65536`           |
| `SERVER_MAX_BODY_BYTES`      | Maximum size of a POST body. A larger body gets `413`.           | `5242880` (5 MiB) |
| `HEALTH_SERVER_ADDR`         | Address of the server for `/metrics`, `/healthz`, and `/readyz`. | `:8080`           |
| `READINESS_CACHE_TTL`        | Time that `/readyz` keeps the result of the upstream probe.      | `30s`             |
| `PPROF_ENABLED`              | Adds `/debug/pprof/*` to the health server. Not for production.  | `false`           |
| `LOG_LEVEL`                  | Log level: `debug`, `info`, `warn`, or `error`.                  | `info`            |
| `LOG_FORMAT`                 | Set to `text` for human-readable output instead of JSON.         | JSON              |

### Endpoints

| Endpoint           | Port           | Purpose                                                                   |
| ------------------ | -------------- | ------------------------------------------------------------------------- |
| `/`                | `8888`         | ExternalDNS negotiation. Returns an empty domain filter.                  |
| `/records`         | `8888`         | ExternalDNS `GET` (list) and `POST` (apply changes).                      |
| `/adjustendpoints` | `8888`         | ExternalDNS endpoint adjustment.                                          |
| `/healthz`         | `8888`, `8080` | Liveness: `200 OK` while the process runs.                                |
| `/readyz`          | `8888`, `8080` | Readiness: probes NetBird and keeps the result for `READINESS_CACHE_TTL`. |
| `/metrics`         | `8080`         | Prometheus metrics.                                                       |
| `/debug/pprof/`    | `8080`         | Go pprof endpoints. Requires `PPROF_ENABLED=true`.                        |

Both ports serve `/healthz` and `/readyz`. The ExternalDNS Helm chart gives the webhook container one port, `8080`, with the name `http-webhook`. The chart sends the Kubernetes probes and the ServiceMonitor to this port, which is the health server. The chart values cannot change this container port, so keep `HEALTH_SERVER_ADDR` on port `8080`.

## Development

`.mise/config.toml` pins the tool versions. To install the tools and run the tests and the linter, use these commands:

```sh
mise install
mise run test
mise run test-e2e
mise run lint
```
