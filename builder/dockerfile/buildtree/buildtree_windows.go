package buildtree

import "strings"

// equalEnvKeys compare two strings and returns true if they are equal. On
// Windows this comparison is case insensitive.
func equalEnvKeys(from, to string) bool {
	return strings.ToUpper(from) == strings.ToUpper(to)
}
