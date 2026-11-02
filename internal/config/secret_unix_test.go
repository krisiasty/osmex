//go:build unix

package config

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSecretFileFIFO(t *testing.T) {
	// Opening a FIFO without a writer blocks, so this test hangs if the file is opened before it is checked.
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadSecret(secretEnv(map[string]string{"OS_APPLICATION_CREDENTIAL_SECRET_FILE": fifo}))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v", err)
	}
}
