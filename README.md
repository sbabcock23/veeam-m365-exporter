# Veeam Backup for Microsoft 365 Prometheus Exporter

A small Go Prometheus exporter for Veeam Backup for Microsoft 365 REST API v8/8.5.

## Features

- Multiple independent VB365 REST API instances in one exporter.
- Per-instance username/password authentication.
- Credentials can be supplied directly in YAML or through environment variables.
- Per-instance `insecure_skip_verify` for self-signed/internal TLS certificates.
- OAuth access-token caching and refresh-token renewal.
- Concurrent collection across configured Veeam instances.
- Pagination up to the API's 10,000-object page size.
- `/metrics` Prometheus endpoint and `/healthz` process health endpoint.
- Multi-stage Docker build with a non-root distroless runtime image.

## Veeam prerequisites

Enable the Veeam Backup for Microsoft 365 REST API service. The exporter expects the v8 API and authenticates with `POST /v8/token` using the password grant, then sends the returned access token as a Bearer token. The normal VB365 REST API port is 4443.

The API account must have sufficient VB365 permissions to read Jobs, JobSessions, BackupRepositories and Proxies.

## Configuration

Copy the example:

```sh
cp config.example.yml config.yml
```

Example:

```yaml
listen_address: ":9810"
scrape_timeout: 25s

instances:
  - name: vb365-primary
    url: https://vb36501.example.com:4443
    username: DOMAIN\\svc_prometheus
    password_env: VB365_PRIMARY_PASSWORD
    insecure_skip_verify: true
    api_version: v8

  - name: vb365-dr
    url: https://vb365-dr.example.com:4443
    username_env: VB365_DR_USERNAME
    password_env: VB365_DR_PASSWORD
    insecure_skip_verify: false
    api_version: v8
```

Credential fields supported per instance are `username`, `password`, `username_env`, and `password_env`. If an `_env` field is specified, its environment-variable value overrides the direct value.

`insecure_skip_verify: true` disables TLS certificate verification only for that VB365 instance. Use it for self-signed certificates only when necessary.

## Build and run with Docker

```sh
docker build -t veeam-vb365-exporter:latest .

docker run -d \
  --name veeam-vb365-exporter \
  -p 9810:9810 \
  -e VB365_PRIMARY_PASSWORD='your-password' \
  -v "$PWD/config.yml:/etc/veeam-vb365-exporter/config.yml:ro" \
  veeam-vb365-exporter:latest
```

Or:

```sh
docker compose up -d --build
```

## Prometheus configuration

```yaml
scrape_configs:
  - job_name: veeam-vb365
    scrape_interval: 60s
    scrape_timeout: 30s
    static_configs:
      - targets:
          - veeam-vb365-exporter:9810
```

All Veeam-specific metrics include an `instance` label matching the configured instance name, so one exporter target can represent many Veeam installations.

## Metrics

Core exporter metrics:

- `veeam_vb365_up{instance}`
- `veeam_vb365_scrape_duration_seconds{instance}`
- `veeam_vb365_scrape_errors{instance}`

Jobs/latest sessions:

- `veeam_vb365_jobs_total`
- `veeam_vb365_job_schedule_enabled`
- `veeam_vb365_job_last_status{status=...}`
- `veeam_vb365_job_last_progress_percent`
- `veeam_vb365_job_last_start_timestamp_seconds`
- `veeam_vb365_job_last_end_timestamp_seconds`
- `veeam_vb365_job_last_duration_seconds`
- `veeam_vb365_job_last_transferred_bytes`
- `veeam_vb365_job_last_processed_objects`
- `veeam_vb365_job_last_processing_rate_bytes_per_second`
- `veeam_vb365_job_last_read_rate_bytes_per_second`
- `veeam_vb365_job_last_write_rate_bytes_per_second`
- `veeam_vb365_job_last_retry_count`

Repositories/proxies:

- `veeam_vb365_repositories_total`
- `veeam_vb365_repository_info`
- `veeam_vb365_proxies_total`
- `veeam_vb365_proxy_status`
- `veeam_vb365_proxy_cpu_usage_percent`
- `veeam_vb365_proxy_memory_usage_percent`
- `veeam_vb365_proxy_maintenance_mode`

## Useful PromQL

Failed latest jobs:

```promql
veeam_vb365_job_last_status{status="Failed"} == 1
```

Jobs whose most recent completed session is older than 24 hours:

```promql
time() - veeam_vb365_job_last_end_timestamp_seconds > 86400
```

Offline proxies:

```promql
veeam_vb365_proxy_status{status!="Online"} == 1
```

High proxy CPU:

```promql
veeam_vb365_proxy_cpu_usage_percent > 90
```

## Notes

`/healthz` indicates that the exporter process is alive. `veeam_vb365_up` indicates whether all enabled API collection sections succeeded for a specific VB365 server during the Prometheus scrape.
