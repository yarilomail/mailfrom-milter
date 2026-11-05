# mailfrom-milter

<table><tr>
<td><img src="https://raw.githubusercontent.com/yarilomail/mailfrom-milter/main/doc/dockerhub-icon.svg" width="110" alt="mailfrom-milter logo"/></td>
<td>

Postfix **milter** written in Go that enforces alignment between the SMTP envelope sender (`MAIL FROM`) and the `From:` message header.

Licensed under **AGPLv3**, see [LICENSE](LICENSE).

[![CI](https://github.com/yarilomail/mailfrom-milter/actions/workflows/ci.yaml/badge.svg)](https://github.com/yarilomail/mailfrom-milter/actions/workflows/ci.yaml)
[![License: AGPL v3](https://img.shields.io/badge/License-AGPLv3-blue.svg)](LICENSE)
[![Go version](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go)](app/go/go.mod)
[![Container](https://img.shields.io/badge/ghcr.io-mailfrom--milter-blue?logo=github)](https://github.com/yarilomail/mailfrom-milter/pkgs/container/mailfrom-milter)
[![Docker Hub](https://img.shields.io/docker/pulls/0kaba0/mailfrom-milter?logo=docker&logoColor=white)](https://hub.docker.com/r/0kaba0/mailfrom-milter)

</td>
</tr></table>

## Architecture

![Architecture](https://raw.githubusercontent.com/yarilomail/mailfrom-milter/main/doc/arch-mailfrom.svg)

---

## The problem

When a mail server hosts multiple domains, an authenticated user can set `MAIL FROM` to their own domain but forge the `From:` header with a different domain. The DKIM signer signs using the `From:` domain — producing a valid DKIM signature for a domain the sender does not own.

**This milter rejects such messages before DKIM signing occurs.**

Only authenticated SMTP sessions (SASL) are checked. Unauthenticated connections (inbound MX delivery) pass through without inspection.

---

## Checks

For every authenticated session the milter performs two checks:

| Flag | Check | Values |
|:---|:---|:---|
| `flag_check_auth` | SASL username domain vs `MAIL FROM` domain | `pass` / `fail` |
| `flag_check_data` | `MAIL FROM` domain vs `From:` header domain | `pass` / `fail` |

---

## Actions

Configured via the `MF_ACTION` environment variable.

| Action | On `flag_check_auth` fail | On `flag_check_data` fail | On both pass |
|:---|:---|:---|:---|
| `reject` | `421 4.7.1 … MFC010001` | `421 4.7.1 … MFC010002` | log + accept (+ `X-MF-Envelope-From` if `MF_SENDER_ADD=yes`) |
| `discard` | silent drop, log `MFC020001` | silent drop, log `MFC020002` | log + accept (+ `X-MF-Envelope-From` if `MF_SENDER_ADD=yes`) |
| `quarantine_header` | log + add headers (`X-MF-Quarantine: yes`) | log + add headers (`X-MF-Quarantine: yes`) | log + add headers (`X-MF-Quarantine: no`) |
| `accept` | log only, accept | log only, accept | log only, accept |

Default: `reject`.

---

## Headers added

### `quarantine_header` action

| Header | Value |
|:---|:---|
| `X-MF-Envelope-From` | `MAIL FROM` address |
| `X-MF-From` | Address extracted from `From:` header |
| `X-MF-Quarantine` | `yes` if any check failed, `no` if all passed |

### `reject` and `discard` actions (when `MF_SENDER_ADD=yes`)

When `MF_SENDER_ADD=yes`, accepted authenticated messages (both checks passed) get:

| Header | Value |
|:---|:---|
| `X-MF-Envelope-From` | `MAIL FROM` address |

---

## Log format

Every processed authenticated message produces one JSON log entry:

```json
{
  "time": "...",
  "level": "INFO",
  "msg": "milter",
  "queue_id": "AE4F61C005B",
  "envelope_from": "user@attacker.com",
  "auth_user": "user@attacker.com",
  "flag_check_auth": "pass",
  "from_header": "ceo@victim.com",
  "flag_check_data": "fail",
  "return_code": "reject"
}
```

`queue_id` matches the Postfix queue ID and can be used to correlate milter log entries with Postfix logs. Empty for messages rejected in the `MAIL FROM` phase (before headers are received).

`return_code` values: `reject`, `discard`, `accept`. (For `quarantine_header`, `return_code` is always `accept`; use the `X-MF-Quarantine` header or `mailfrom_messages_total{action="quarantine"}` metric to detect flagged messages.)

---

## Postfix configuration

```
smtpd_milters = inet:mailfrom-milter.mail.svc.cluster.local:10031
milter_mail_macros = i {mail_addr} {client_addr} {client_name} {auth_authen} {auth_type}
milter_default_action = accept
```

`{auth_authen}` must be present in `milter_mail_macros` (included in Postfix defaults).

---

## Observability

### HTTP endpoints (port 8081)

| Path | Description |
|:---|:---|
| `/healthz` | Liveness — always `200 OK` while the process is running |
| `/readyz` | Readiness — `200 OK` after the milter socket is bound, `503` during shutdown |
| `/metrics` | Prometheus metrics in text format |

### Prometheus metrics

**`mailfrom_connections_total`** — counter, total SMTP connections accepted.

**`mailfrom_messages_total`** — counter, SMTP messages processed.

Labels:

| Label | Values | Description |
|:---|:---|:---|
| `action` | `accept` / `reject` / `discard` / `quarantine` | Final disposition |
| `check_auth` | `pass` / `fail` / `skip` | SASL username domain vs `MAIL FROM` domain |
| `check_data` | `pass` / `fail` / `skip` | `MAIL FROM` domain vs `From:` header domain |

`skip` means the check was not reached (unauthenticated session, or action decided before the check ran).

Example query — rejection rate over 5 minutes:

```promql
rate(mailfrom_messages_total{action="reject"}[5m])
```

---

## Environment variables

| Variable | Default | Description |
|:---|:---|:---|
| `LISTEN_ADDR` | `0.0.0.0:10031` | TCP address for the milter socket |
| `METRICS_ADDR` | `0.0.0.0:8081` | TCP address for `/healthz`, `/readyz`, `/metrics` |
| `MF_ACTION` | `reject` | `reject` / `discard` / `quarantine_header` / `accept` |
| `REJECT_CODE` | `421` | SMTP reply code for `reject` action: `421` (temp) or `550` (perm) |
| `MF_SENDER_ADD` | `no` | Set to `yes` to add `X-MF-Envelope-From` on accepted authenticated messages (`reject` and `discard` actions only) |
| `MILTER_READ_TIMEOUT_SECONDS` | `3600` | A milter connection silent this long is closed; Postfix keeps one per SMTP session and is silent on it while the client sends |
| `MILTER_WRITE_TIMEOUT_SECONDS` | `60` | A reply Postfix does not take this long closes the connection |
| `LOG_LEVEL` | — | Set to `debug` for verbose per-message logging |

---

## Stack

- Go 1.27
- [`0kaba0hub/go-milter`](https://github.com/0kaba0hub/go-milter) v0.5.0: fork of `emersion/go-milter` with `slog` logging and `sync.Pool` write buffer (module renamed to `github.com/0kaba0hub/go-milter`)
- [`0kaba0hub/go-message`](https://github.com/0kaba0hub/go-message) v0.19.0: fork of `emersion/go-message` (indirect dep of go-milter, module renamed to `github.com/0kaba0hub/go-message`)
- Both forks have weekly upstream release monitors
- Alpine 3.24 runtime image

---

## Directory layout

```
app/go/
|-  main.go
|-  metrics.go
|-  Dockerfile
|-  go.mod
\-  go.sum
helm/
|-  Chart.yaml
|-  values.yaml
\-  templates/
    |-  deployment.yaml
    \-  service.yaml
helm_values/
\-  values-sandbox.yaml
argocd-app.yaml
.github/workflows/ci.yaml
```

---

## Post-deploy smoke test

A Helm post-install/post-upgrade hook Job that verifies all four milter cases after every deployment.

| Case | Auth | MAIL FROM | From: header | Expected |
|:---|:---|:---|:---|:---|
| 1 | none | own domain | own domain | accept (skip) |
| 2 | yes | **other** domain | — | reject MFC010001 |
| 3 | yes | own domain | **other** domain | reject MFC010002 |
| 4 | yes | own domain | own domain | accept |

The Job skips automatically if the credentials secret is absent.

**Setup (one-time, outside Helm):**

```sh
kubectl create secret generic mailfrom-test-creds \
  --from-literal=TEST_SMTP_HOST='relay.relay.svc.cluster.local' \
  --from-literal=TEST_SMTP_PORT='587' \
  --from-literal=TEST_SMTP_PORT25='25' \
  --from-literal=TEST_SASL_USER='noreply@mail.example.com' \
  --from-literal=TEST_SASL_PASSWORD='<smtp-password>' \
  --from-literal=TEST_MAIL_FROM='noreply@mail.example.com' \
  --from-literal=TEST_MAIL_TO='test@mail.example.com' \
  -n <namespace>
```

**Enable in values:**

```yaml
postDeployTest:
  enabled: true
  credentialsSecret: "mailfrom-test-creds"
```

**Check results:**

```sh
kubectl logs -l job-name=mailfrom-post-deploy-test -n <namespace>
```

---

## Deploy

### Kubernetes (ArgoCD)

```sh
kubectl apply -f argocd-app.yaml
```

### Local

```sh
docker build -t mailfrom-milter:dev app/go/
docker run --rm -p 10031:10031 -e MF_ACTION=accept -e LOG_LEVEL=debug mailfrom-milter:dev
```

---

## CI

Every push to `main` triggers lint → test → build:

| Trigger | Image tags | Release |
|:---|:---|:---|
| Push to `main` | `<sha>` + `latest` | — |
| Push to `main` with new `appVersion` in `helm/Chart.yaml` | `<sha>` + `latest` + `v{appVersion}` | GitHub Release created automatically |

The sandbox values file (`helm_values/values-sandbox.yaml`) is always updated with the short SHA of the latest build.

### Releasing a new version

1. Bump `appVersion` in `helm/Chart.yaml` in your PR (e.g. `"1.2.0"`)
2. Merge to `main`
3. CI automatically creates git tag `v1.2.0`, GitHub Release, and pushes the versioned image
