// Package host is the daemon: it owns the images, the desks, and the API.
package host

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
)

// DefaultDir is where hollow keeps everything: images, desks, the token.
//
// HOLLOW_HOME moves it. Otherwise root gets the system location and anybody
// else gets their own, so that a daemon run from a shell and one run as a
// service do not fight over one directory.
func DefaultDir() string {
	if v := os.Getenv("HOLLOW_HOME"); v != "" {
		return v
	}
	if os.Geteuid() == 0 {
		return "/var/lib/hollow"
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "hollow")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "hollow"
	}
	return filepath.Join(home, ".local", "share", "hollow")
}

// Token loads the bearer token clients must present, minting one the first
// time. The file is readable by its owner only, which is the whole access
// story on the machine itself: reading it is being allowed in.
func Token(dir string) (string, error) {
	path := filepath.Join(dir, "token")
	if data, err := os.ReadFile(path); err == nil {
		tok := string(trimSpace(data))
		if tok != "" {
			return tok, nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// ReadToken reads an existing token without creating one.
func ReadToken(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return "", err
	}
	tok := string(trimSpace(data))
	if tok == "" {
		return "", errors.New("the token file is empty")
	}
	return tok, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
