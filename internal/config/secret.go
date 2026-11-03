package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/krisiasty/osmex/internal/secret"
)

// SystemdCredential is the systemd credential that holds the application credential secret: osmex reads
// $CREDENTIALS_DIRECTORY/application-credential-secret, which LoadCredentialEncrypted= in the example unit provides.
const SystemdCredential = "application-credential-secret"

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
	s, err := secret.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", source, err)
	}
	e.CredentialSecret = s
	return nil
}
