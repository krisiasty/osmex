# Credentials for osmex

osmex authenticates with a Keystone application credential that belongs to the customer project. The credential is project-bound, so osmex sends no scope, and it only needs
read access.

## Create the application credential

Run this as a user with access to the customer project:

```sh
openstack application credential create osmex \
  --role reader \
  --description "osmex exporter, read-only" \
  --access-rules "$(cat access-rules.json)"   # optional, see below
```

The command prints the credential id and secret. The secret is shown only once; retain it in your secret store. Put the id and the non-secret OpenStack settings
into `/etc/osmex/osmex.env`, using [deploy/systemd/osmex.env.example](../deploy/systemd/osmex.env.example). Keep the secret separate, as described below.
Horizon can also generate an openrc: copy the non-secret values without the `export ` prefixes.

## Supply the secret through systemd-creds

The example unit uses `LoadCredentialEncrypted=` on Ubuntu 24.04, Debian 12 or later. systemd decrypts the secret when starting the service and makes a private
runtime copy readable by its dynamic user. osmex reads that file from the directory in `$CREDENTIALS_DIRECTORY`; the secret value is not placed in its environment.
This follows the credential-file pattern used by [arex](https://github.com/krisiasty/arex/blob/main/docs/install-systemd.md).

Prepare the configuration on the machine that will run osmex:

```sh
sudo install -d -o root -g root -m 0755 /etc/osmex
sudo install -o root -g root -m 0600 deploy/systemd/osmex.env.example /etc/osmex/osmex.env
sudoedit /etc/osmex/osmex.env
```

Set `OS_AUTH_URL`, `OS_APPLICATION_CREDENTIAL_ID`, region and interface as appropriate. Do not put `OS_APPLICATION_CREDENTIAL_SECRET` or
`OS_APPLICATION_CREDENTIAL_SECRET_FILE` in that file: while the systemd credential is loaded, osmex refuses to start if either is set, rather than let one
replace the encrypted secret. A custom CA bundle named by `OS_CACERT` must be readable by the service's dynamic user.

Encrypt the secret with a hidden prompt, so its value is not entered as a shell command or written to a plaintext file. Run this in Bash:

```bash
(
  set -o pipefail
  umask 077
  cred=/etc/osmex/application-credential-secret.cred
  sudo -v &&
    systemd-ask-password 'OpenStack application credential secret:' |
    sudo systemd-creds encrypt --with-key=host --name=application-credential-secret - "$cred.new" &&
    sudo chmod 0400 "$cred.new" &&
    sudo mv "$cred.new" "$cred"
) || sudo rm -f /etc/osmex/application-credential-secret.cred.new
```

The explicit `--name` must match the credential name in the unit, even though the encrypted file has a `.cred` suffix. The temporary output is moved into
place only after encryption succeeds, and removed if any step fails. The subshell keeps `pipefail` and the `umask` out of your shell; sudo combines the
`umask` with its own, so the temporary file is never readable by other users. Repeat the command to replace the encrypted secret.

`--with-key=host` uses `/var/lib/systemd/credential.secret`, generated automatically if needed, and works without a TPM. `--with-key=auto` can also bind
decryption to the local TPM when available. Encrypt on the target host and keep the original secret in your secret store so you can provision a replacement host.
Host-key encryption does not protect against someone who has both the encrypted file and the host key, or root access on the running host.
See [systemd-creds(1)](https://manpages.debian.org/bookworm/systemd/systemd-creds.1.en.html).

The relevant settings in [deploy/systemd/osmex.service](../deploy/systemd/osmex.service) are:

```ini
EnvironmentFile=/etc/osmex/osmex.env
LoadCredentialEncrypted=application-credential-secret:/etc/osmex/application-credential-secret.cred
```

systemd sets `CREDENTIALS_DIRECTORY` to the unit's credential directory, normally `/run/credentials/osmex.service`, and osmex reads the decrypted
`application-credential-secret` from it. The credential name must stay `application-credential-secret`. osmex reads the secret at startup, removing trailing
CR/LF characters while preserving other whitespace. A missing, unreadable or empty secret, one with only whitespace, and anything but a regular file of at most
4 KiB fail startup. Secrets are never included in configuration error messages.

Install and start the unit after preparing the configuration and encrypted credential:

```sh
sudo install -o root -g root -m 0644 deploy/systemd/osmex.service /etc/systemd/system/osmex.service
sudoedit /etc/systemd/system/osmex.service
sudo systemd-analyze verify /etc/systemd/system/osmex.service
sudo systemctl daemon-reload
sudo systemctl enable osmex.service
sudo systemctl restart osmex.service
```

Adjust the service flags in `ExecStart` before starting. Once authentication succeeds, `/metrics` includes `openstack_exporter_auth_up 1`.

After replacing the encrypted secret or changing the application credential id, restart osmex: systemd's credential copy and osmex's loaded secret are refreshed
only at startup. An application credential's secret cannot be changed; create a replacement credential and update its id and encrypted secret together.

### The scrape password and TLS key under systemd

Like the application credential secret, the scrape password and the TLS private key are stored encrypted and reach osmex as credentials. Encrypt the
scrape password with the command above, using `cred=/etc/osmex/scrape-password.cred`, `--name=scrape-password` and the prompt
`'osmex scrape password:'`.

Encrypt the private key from its PEM file, then delete the plaintext copy:

```bash
(
  set -o pipefail
  umask 077
  key=/path/to/tls.key
  cred=/etc/osmex/tls-key.cred
  sudo systemd-creds encrypt --with-key=host --name=tls-key "$key" "$cred.new" &&
    sudo chmod 0400 "$cred.new" &&
    sudo mv "$cred.new" "$cred" &&
    sudo rm -f "$key"
) || sudo rm -f /etc/osmex/tls-key.cred.new
```

The plaintext is deleted only after the encrypted file is in place. Where a tool renews the key, run the same encryption from its deploy hook, followed
by `systemctl restart osmex`, and keep the tool's own copy of the key under the same protection as any other private key.

Install the certificate, which is public, at `/etc/osmex/tls.crt` with mode 0644, then use the commented-out lines in
[deploy/systemd/osmex.service](../deploy/systemd/osmex.service):

```ini
LoadCredentialEncrypted=tls-key:/etc/osmex/tls-key.cred
LoadCredentialEncrypted=scrape-password:/etc/osmex/scrape-password.cred
ExecStart=/usr/local/bin/osmex -nova -cinder -neutron -listen-address=:9180 -listen-tls-cert-file=/etc/osmex/tls.crt -listen-tls-key-file=%d/tls-key -listen-auth-username=prometheus -listen-auth-password-file=%d/scrape-password
```

`%d` is the unit's credential directory. systemd decrypts the copies at start, so restart osmex after renewing the key or rotating the password.
Renew the certificate and key together: a new certificate on disk with the old key in the credential directory doesn't load, and osmex keeps serving
the previous certificate until the restart.

For manual runs, `OS_APPLICATION_CREDENTIAL_SECRET` remains supported. Alternatively, set `OS_APPLICATION_CREDENTIAL_SECRET_FILE` to a readable plaintext
secret file, such as a container secret; systemd is not required for file-based loading.

## Optional access rules

Access rules restrict the credential to the API calls osmex makes. Include only the OpenStack services you enable, plus the `identity` rule, which lets osmex
read the credential's own expiry whenever any service is enabled. Paths are matched against the request path seen by each
service, so verify them on your cloud before relying on them: if a rule does not match, the affected module reports `openstack_exporter_api_errors_total{code="401"}` or
`{code="403"}`.

```json
[
  {"service": "identity", "method": "GET", "path": "/v3/users/*/application_credentials/*"},
  {"service": "compute", "method": "GET", "path": "/v2.1/servers/detail"},
  {"service": "compute", "method": "GET", "path": "/v2.1/servers/*/os-instance-actions"},
  {"service": "compute", "method": "GET", "path": "/v2.1/limits"},
  {"service": "block-storage", "method": "GET", "path": "/v3/*/volumes/detail"},
  {"service": "block-storage", "method": "GET", "path": "/v3/*/snapshots/detail"},
  {"service": "block-storage", "method": "GET", "path": "/v3/*/backups/detail"},
  {"service": "block-storage", "method": "GET", "path": "/v3/*/limits"},
  {"service": "network", "method": "GET", "path": "/v2.0/ports"},
  {"service": "network", "method": "GET", "path": "/v2.0/floatingips"},
  {"service": "network", "method": "GET", "path": "/v2.0/quotas/*/details"},
  {"service": "load-balancer", "method": "GET", "path": "/v2/lbaas/loadbalancers"},
  {"service": "load-balancer", "method": "GET", "path": "/v2/lbaas/listeners"},
  {"service": "load-balancer", "method": "GET", "path": "/v2/lbaas/loadbalancers/*/status"},
  {"service": "load-balancer", "method": "GET", "path": "/v2/lbaas/quotas/*"},
  {"service": "key-manager", "method": "GET", "path": "/v1/secrets"}
]
```

If your catalog lists Cinder as `volumev3` instead of `block-storage`, use that service type in the rules.
