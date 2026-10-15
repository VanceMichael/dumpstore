package smb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Usershare is one Samba usershare file in dumpstore's managed usershare
// directory. The file name (written by `net usershare add`) is the share name.
type Usershare struct {
	Name string
	Path string
}

// ListUsershares reads every usershare definition directly from the
// dumpstore-managed usershare directory. The directory is owned exclusively
// by dumpstore (see UsershareDir), so its files are an authoritative set
// that does not depend on parsing `net usershare info` exit codes.
//
// A missing directory means SMB has never been initialised and yields a nil
// slice with no error. Any other read failure, or a malformed usershare file
// without a parseable path, is returned so callers (the pre-destroy reference
// guard) can fail closed.
func ListUsershares(goos string) ([]Usershare, error) {
	return listUsersharesDir(UsershareDir(goos))
}

func listUsersharesDir(dir string) ([]Usershare, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read usershare dir %s: %w", dir, err)
	}

	var shares []Usershare
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read usershare %s: %w", e.Name(), err)
		}
		path, ok := parseUsersharePath(data)
		if !ok {
			return nil, fmt.Errorf("usershare %q has no parseable path", e.Name())
		}
		shares = append(shares, Usershare{Name: e.Name(), Path: path})
	}
	return shares, nil
}

// parseUsersharePath extracts the path= value from a `net usershare` file.
// File layout (stable Samba format), one key=value per line:
//
//	#VERSION 0
//	path=/tank/data
//	comment=
//	usershare_acl=S-1-1-0:F
//	guest_ok=n
func parseUsersharePath(data []byte) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if strings.TrimSpace(key) == "path" {
			val = strings.TrimSpace(val)
			return val, val != ""
		}
	}
	return "", false
}
