# Kubernetes deployment

Use [deploy/kubernetes/osmex.yaml](../deploy/kubernetes/osmex.yaml) as a Deployment and ClusterIP Service example. Set a published image version,
the OpenStack URL, region and interface, and the services to export before applying it. The example version is a placeholder for a release with container images.
Apply all resources in your chosen application namespace.

## Secrets from an external store

osmex reads sensitive values from mounted files. It needs no Kubernetes API access, secret-store client, or store credentials of its own.
An operator such as [External Secrets Operator](https://external-secrets.io/latest/api/externalsecret/) synchronizes your external store into these Kubernetes Secrets:

| Kubernetes Secret | Keys | Use |
| --- | --- | --- |
| `osmex-openstack` | `application-credential-id`, `application-credential-secret` | The id is non-secret configuration; only the secret is mounted as a file |
| `osmex-scrape` | `password` | Password required by the metrics endpoint |
| `osmex-tls` | `tls.crt`, `tls.key` | PEM server certificate/chain and private key; Secret type `kubernetes.io/tls` |

[deploy/kubernetes/external-secrets.yaml](../deploy/kubernetes/external-secrets.yaml) contains optional mappings for all three. The examples use
`external-secrets.io/v1` and assume ESO and a `SecretStore` named `secret-store` already exist in the application namespace. Set `secretStoreRef` and
the remote keys/properties for your provider. Configure the operator's store authentication separately, preferably through workload identity where supported.
The mappings contain references only; keep passwords, private keys and application credential secrets in the external store.
Protect the synchronized Kubernetes Secrets with namespace RBAC and the cluster's encryption-at-rest configuration; ESO creates a copy in Kubernetes.

Another controller or a secret volume provider can supply the same files. If cert-manager manages server certificates, let it create `osmex-tls` and
omit the TLS ExternalSecret so only one controller owns that Secret. The certificate must cover the service DNS names the scraper uses, such as
`osmex.<namespace>.svc` and the fully qualified service name for your cluster domain. Configure the scraper to trust its issuing CA.

Wait until the Secrets have been synchronized before starting the Deployment. The required Secret mounts prevent the container from starting with missing Secrets.

## Mounted files and permissions

The Deployment mounts whole Secret directories read-only, with `defaultMode: 0440`, and runs as UID/GID `65532` with `fsGroup: 65532`.
This makes the files readable by osmex's group without making them world-readable. The application credential secret, scrape password and TLS private key
are not supplied as environment values or command-line values; environment variables and flags contain their file paths only. The credential id is not secret.

Do not use `subPath` mounts: Kubernetes does not propagate Secret updates to those mounts. Whole-directory Secret volumes receive updates eventually,
which lets osmex observe rotated files. See [Kubernetes Secrets](https://kubernetes.io/docs/concepts/configuration/secret/) and
[pod security contexts](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/).

The example disables service-account token mounting, drops capabilities and uses a read-only root filesystem. If OpenStack requires a custom CA,
mount that public CA bundle from a ConfigMap or Secret and set `OS_CACERT` to its mounted path.

## Rotation

ESO's synchronization interval and Kubernetes volume propagation both add delay before a changed file reaches osmex.

- **Scrape password:** the first request carrying Basic auth credentials after the 30-second reload interval re-reads the file before checking them. The previous password
  remains accepted until that reload. Synchronize the scraper's password from the same source; a brief mismatch while the two sides refresh can produce HTTP 401s.
- **Server certificate and key:** a new TLS handshake checks for changes at most every 30 seconds and reloads a valid pair. A broken or mismatched pair leaves
  the previous pair in use. Renew the certificate and key together in `osmex-tls`.
- **OpenStack application credential:** osmex reads it only at startup. Arrange a pod rollout when `osmex-openstack` changes; ESO synchronization alone does not
  restart the pod. Replace the credential id and secret together, because a Keystone application credential's secret cannot be changed independently.
- **Client CA bundle for optional mutual TLS:** if configured, it is read at startup and also requires a pod rollout after changes.

The image contains no secret material. `systemd-creds` remains the secret delivery mechanism for the separate systemd deployment; Kubernetes supplies the pod's files.

## Scraping and probes

The Service exposes HTTPS metrics on port `9180`, with basic authentication. Configure the scraper's CA, server name and username (`prometheus` in the example),
and supply its password through that scraper's secret integration. If it runs in another namespace, synchronize its own Secret from the same external source.

Kubernetes probes use a separate plain-HTTP listener on port `9181`, serving only `/livez` and `/readyz`. The Service does not expose that port.
The example has no startup probe. `/livez` passes from the start and fails only when polling stops, so it needs no startup grace period. A startup
probe on `/readyz` would restart the pod whenever its first polls take longer than the probe's budget, as they can in a large project, and turn a
slow OpenStack API into a restart loop. The pod stays out of the Service until `/readyz` turns ready. OpenStack API failures remain visible through
metrics without making a previously ready exporter unready.
