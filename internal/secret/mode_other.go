//go:build !unix

package secret

import "os"

// fileGroupIsOurs cannot tell without Unix groups, so group-readable counts as loose.
func fileGroupIsOurs(os.FileInfo) bool { return false }
