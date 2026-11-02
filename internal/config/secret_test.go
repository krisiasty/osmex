package config

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureSecret is written to secret files in error cases; no error may contain it.
const fixtureSecret = "fixture-secret-must-not-appear-in-errors"

func writeSecret(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// secretEnv returns fullEnv without the inline secret, with vars added; an empty value deletes the variable.
func secretEnv(vars map[string]string) map[string]string {
	m := maps.Clone(fullEnv)
	delete(m, "OS_APPLICATION_CREDENTIAL_SECRET")
	for k, v := range vars {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}

func loadSecret(vars map[string]string) (Config, error) {
	c, err := Parse([]string{"-nova"}, env(vars))
	if err != nil {
		return c, err
	}
	return c, c.LoadSecret()
}

func TestSecretFile(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"no newline", "file-secret", "file-secret"},
		{"newline", "file-secret\n", "file-secret"},
		{"CRLF", "file-secret\r\n", "file-secret"},
		{"preserve spaces", " file-secret \n", " file-secret "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := writeSecret(t, t.TempDir(), "secret", tc.content)
			c, err := loadSecret(secretEnv(map[string]string{"OS_APPLICATION_CREDENTIAL_SECRET_FILE": file}))
			if err != nil {
				t.Fatal(err)
			}
			if c.OS.CredentialSecret != tc.want {
				t.Error("secret file was not loaded as expected")
			}
		})
	}
}

func TestParseReadsNoSecretFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	vars := secretEnv(map[string]string{"OS_APPLICATION_CREDENTIAL_SECRET_FILE": missing})
	c, err := Parse([]string{"-nova"}, env(vars))
	if err != nil {
		t.Fatalf("Parse read the secret file: %v", err)
	}
	if c.OS.CredentialSecretFile != missing || c.OS.CredentialSecret != "" {
		t.Errorf("os env = %+v", c.OS)
	}
	// Without services LoadSecret has nothing to do.
	for _, args := range [][]string{nil, {"-version"}} {
		c, err := Parse(args, env(vars))
		if err == nil {
			err = c.LoadSecret()
		}
		if err != nil {
			t.Errorf("%v: secret file should not be needed: %v", args, err)
		}
	}
}

func TestSecretFileValidatedAfterEnv(t *testing.T) {
	vars := secretEnv(map[string]string{
		"OS_AUTH_URL":                           "",
		"OS_APPLICATION_CREDENTIAL_SECRET_FILE": filepath.Join(t.TempDir(), "missing"),
	})
	if _, err := loadSecret(vars); err == nil || !strings.Contains(err.Error(), "OS_AUTH_URL") {
		t.Errorf("err = %v, want the OS_AUTH_URL error first", err)
	}
}

func TestSystemdCredential(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, SystemdCredential, "cred-secret\n")
	c, err := loadSecret(secretEnv(map[string]string{"CREDENTIALS_DIRECTORY": dir}))
	if err != nil {
		t.Fatal(err)
	}
	if c.OS.CredentialSecret != "cred-secret" {
		t.Error("systemd credential was not loaded as expected")
	}
}

func TestCredentialsDirectoryWithoutSecret(t *testing.T) {
	// systemd sets CREDENTIALS_DIRECTORY for any credential, so the variables still apply when the secret isn't one.
	dir := t.TempDir()
	writeSecret(t, dir, "other", "other")
	c, err := loadSecret(secretEnv(map[string]string{"CREDENTIALS_DIRECTORY": dir, "OS_APPLICATION_CREDENTIAL_SECRET": "inline"}))
	if err != nil || c.OS.CredentialSecret != "inline" {
		t.Errorf("inline secret not used: err = %v", err)
	}
	if _, err := loadSecret(secretEnv(map[string]string{"CREDENTIALS_DIRECTORY": dir})); !errors.Is(err, errNoSecret) {
		t.Errorf("err = %v, want errNoSecret", err)
	}
}

func TestSecretErrors(t *testing.T) {
	dir := t.TempDir()
	file := writeSecret(t, dir, "secret", fixtureSecret)
	credDir := t.TempDir()
	writeSecret(t, credDir, SystemdCredential, fixtureSecret)
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"no source", nil, "must be set"},
		{"inline and file", map[string]string{"OS_APPLICATION_CREDENTIAL_SECRET": fixtureSecret, "OS_APPLICATION_CREDENTIAL_SECRET_FILE": file}, "set only one"},
		{"credential and inline", map[string]string{"CREDENTIALS_DIRECTORY": credDir, "OS_APPLICATION_CREDENTIAL_SECRET": fixtureSecret}, "is loaded"},
		{"credential and file", map[string]string{"CREDENTIALS_DIRECTORY": credDir, "OS_APPLICATION_CREDENTIAL_SECRET_FILE": file}, "is loaded"},
		{"missing file", map[string]string{"OS_APPLICATION_CREDENTIAL_SECRET_FILE": filepath.Join(dir, "missing")}, "reading OS_APPLICATION_CREDENTIAL_SECRET_FILE"},
		{"directory", map[string]string{"OS_APPLICATION_CREDENTIAL_SECRET_FILE": dir}, "not a regular file"},
		{"empty", map[string]string{"OS_APPLICATION_CREDENTIAL_SECRET_FILE": writeSecret(t, dir, "empty", "\r\n")}, "empty secret"},
		{"whitespace", map[string]string{"OS_APPLICATION_CREDENTIAL_SECRET_FILE": writeSecret(t, dir, "blank", " \t\n")}, "empty secret"},
		{"too large", map[string]string{"OS_APPLICATION_CREDENTIAL_SECRET_FILE": writeSecret(t, dir, "large", strings.Repeat(fixtureSecret, 200))}, "larger than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSecret(secretEnv(tc.vars))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), fixtureSecret) {
				t.Error("error exposed the secret")
			}
		})
	}
}
