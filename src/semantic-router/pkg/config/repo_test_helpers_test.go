package config

import "path/filepath"

// repoRel joins repo-relative config asset paths for the contract tests that
// read files under the repository-root config/ tree. The docs-contract
// helpers this once lived alongside were removed with the website/ tree.
func repoRel(parts ...string) string {
	return filepath.Join(parts...)
}
