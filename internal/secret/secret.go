// Package secret reads secrets from files, and keeps one that can rotate while osmex runs.
package secret

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// maxSize bounds a secret file. Real secrets are far smaller; the bound stops a wrong path, such as a device, from
// being read without end.
const maxSize = 4 << 10

// ReadFile reads a secret from a regular file of at most 4 KiB and removes trailing CR/LF characters, which a shell
// redirect or an editor adds; other whitespace is kept. A file with nothing but whitespace is an error. Errors never
// contain the secret.
func ReadFile(path string) (string, error) {
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
	b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	switch {
	case err != nil:
		return "", err
	case len(b) > maxSize:
		return "", fmt.Errorf("%s is larger than %d bytes", path, maxSize)
	}
	secret := strings.TrimRight(string(b), "\r\n")
	if strings.TrimSpace(secret) == "" {
		return "", fmt.Errorf("%s contains an empty secret", path)
	}
	return secret, nil
}

// Credential is a secret read from a file that can be read again. Secrets rotate underneath a running process: the
// kubelet updates a mounted Kubernetes secret in place, so a secret read only at startup would be rejected after the
// next rotation until someone restarted osmex.
type Credential struct {
	path string

	mu    sync.RWMutex
	value string
}

// NewFileCredential reads the secret at path now, so a wrong path fails at startup.
func NewFileCredential(path string) (*Credential, error) {
	v, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &Credential{path: path, value: v}, nil
}

// Value returns the current secret.
func (c *Credential) Value() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.value
}

// Reload reads the file again and reports whether the secret changed. On failure the secret in memory is kept: a
// half-written file must not replace a secret that worked a moment ago.
func (c *Credential) Reload() (changed bool, err error) {
	v, err := ReadFile(c.path)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v == c.value {
		return false, nil
	}
	c.value = v
	return true, nil
}

// LooseMode reports whether info's permissions let someone other than this process read the file. Other-readable
// always does. Group-readable does only when the group is one this process is not in: Kubernetes mounts a secret as
// root:<fsGroup> with at least 0440, which is as tight as a mounted secret can be, and flagging it would teach operators
// to ignore the warning.
func LooseMode(info os.FileInfo) bool {
	mode := info.Mode().Perm()
	return mode&0o007 != 0 || mode&0o070 != 0 && !fileGroupIsOurs(info)
}
