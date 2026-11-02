package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SystemdCredential is the systemd credential that holds the application credential secret: osmex reads
// $CREDENTIALS_DIRECTORY/application-credential-secret, which LoadCredentialEncrypted= in the example unit provides.
const SystemdCredential = "application-credential-secret"

// maxSecretSize bounds a secret file. Keystone's secrets are far smaller; the bound stops a wrong path, such as a
// device, from being read without end.
const maxSecretSize = 4 << 10

var (
	errBothSecrets = errors.New("set only one of OS_APPLICATION_CREDENTIAL_SECRET and OS_APPLICATION_CREDENTIAL_SECRET_FILE")
	errNoSecret    = errors.New("OS_APPLICATION_CREDENTIAL_SECRET, OS_APPLICATION_CREDENTIAL_SECRET_FILE or the systemd credential " +
		SystemdCredential + " must be set when a service is enabled")
)

// LoadSecret resolves the application credential secret when a service is enabled. It comes from exactly one of the
// systemd credential SystemdCredential, OS_APPLICATION_CREDENTIAL_SECRET_FILE and OS_APPLICATION_CREDENTIAL_SECRET:
// a variable left in an environment file must not silently replace the encrypted credential. A secret file loses
// trailing CR/LF characters and keeps other whitespace. Errors never contain the secret.
func (c *Config) LoadSecret() error {
	if !c.AnyService() {
		return nil
	}
	env := &c.OS
	if env.CredentialsDirectory != "" {
		cred := filepath.Join(env.CredentialsDirectory, SystemdCredential)
		if _, err := os.Stat(cred); !errors.Is(err, fs.ErrNotExist) {
			if env.CredentialSecret != "" || env.CredentialSecretFile != "" {
				return fmt.Errorf("the systemd credential %s is loaded: remove OS_APPLICATION_CREDENTIAL_SECRET and "+
					"OS_APPLICATION_CREDENTIAL_SECRET_FILE from the environment", SystemdCredential)
			}
			return env.loadSecretFile(cred, "systemd credential "+SystemdCredential)
		}
	}
	switch {
	case env.CredentialSecretFile != "":
		return env.loadSecretFile(env.CredentialSecretFile, "OS_APPLICATION_CREDENTIAL_SECRET_FILE")
	case env.CredentialSecret == "":
		return errNoSecret
	}
	return nil
}

func (e *OSEnv) loadSecretFile(path, source string) error {
	secret, err := readSecretFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", source, err)
	}
	e.CredentialSecret = secret
	return nil
}

// readSecretFile reads a secret from a regular file of at most maxSecretSize bytes and removes trailing CR/LF
// characters. A file with nothing but whitespace is an error.
func readSecretFile(path string) (string, error) {
	// Stat before opening: opening a FIFO, such as /dev/stdin fed by a pipe, blocks until a writer appears.
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretSize+1))
	switch {
	case err != nil:
		return "", err
	case len(b) > maxSecretSize:
		return "", fmt.Errorf("%s is larger than %d bytes", path, maxSecretSize)
	}
	secret := strings.TrimRight(string(b), "\r\n")
	if strings.TrimSpace(secret) == "" {
		return "", fmt.Errorf("%s contains an empty secret", path)
	}
	return secret, nil
}
