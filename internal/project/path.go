package project

import (
	"path/filepath"
	"regexp"
	"strings"
)

// driveLetterRe matches a Windows drive specifier such as "C:" or "c:". A
// drive-relative value like "C:work" is not absolute on Windows, yet it
// resolves against the per-drive current directory, so it is rejected
// everywhere.
var driveLetterRe = regexp.MustCompile(`^[A-Za-z]:`)

// IsProjectRelative reports whether p is a non-empty path that keeps a command
// inside the project root on every platform.
//
// filepath.IsAbs alone is not enough: Windows treats "/tmp" and "\foo" as
// rooted but not absolute, so both would pass an IsAbs-only check and then
// resolve outside the project once joined to the root. The predicate therefore
// rejects, independently of the host platform:
//
//	the empty string (a present path field must name something)
//	native absolute paths (filepath.IsAbs)
//	anything starting with "/" or "\" (POSIX absolute and Windows rooted
//	paths, including UNC "\\server\share")
//	drive-letter forms such as `C:\x` and `C:/x` (^[A-Za-z]:)
//	any path segment equal to "..", splitting on both "/" and "\" so that
//	"../x" and "..\x" are both caught on every platform
//
// It accepts ".", "sub", "sub/dir" and "./x".
func IsProjectRelative(p string) bool {
	switch {
	case p == "":
		return false
	case filepath.IsAbs(p):
		return false
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, `\`):
		return false
	case driveLetterRe.MatchString(p):
		return false
	}
	for _, segment := range strings.FieldsFunc(p, isPathSeparator) {
		if segment == ".." {
			return false
		}
	}
	return true
}

// isPathSeparator splits on both separators so traversal is detected the same
// way on every platform: on Linux "..\x" is one meaningless file name to
// filepath, while Windows reads it as a parent directory.
func isPathSeparator(r rune) bool { return r == '/' || r == '\\' }

// slashPath rewrites both separators to "/" so that comparisons and splitting
// are platform-independent ("path" only understands "/").
func slashPath(p string) string { return strings.ReplaceAll(p, "\\", "/") }
