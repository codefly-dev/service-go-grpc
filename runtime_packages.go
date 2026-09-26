package main

import (
	"fmt"
	"regexp"
)

// runtimePackageName is the only shape a runtime package entry may take: a
// plain Alpine package name. The entry is interpolated into the final stage's
// `RUN apk add` line, so anything beyond a bare name is refused rather than
// escaped — a version constraint ("git=2.45", "git>2") would make the recipe's
// result depend on the repository state at build time, and a space, quote or
// shell metacharacter would add arguments or commands to the build.
var runtimePackageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]{0,63}$`)

// validateRuntimePackages refuses a runtime-packages list that the final stage
// could not install exactly as declared. A bad entry fails the load or the
// recipe render; it is never dropped, because a silently missing package
// surfaces only when the service first shells out to it in production.
func validateRuntimePackages(packages []string) error {
	seen := make(map[string]bool, len(packages))
	for _, name := range packages {
		if !runtimePackageName.MatchString(name) {
			return fmt.Errorf("runtime-packages: %q must be a plain Alpine package name matching %s (no version pins, spaces or shell characters)", name, runtimePackageName)
		}
		if seen[name] {
			return fmt.Errorf("runtime-packages: duplicate package %q", name)
		}
		seen[name] = true
	}
	return nil
}
