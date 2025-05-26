package anduin

import (
	"fmt"

	"cuelang.org/go/cue/ast"
	"cuelang.org/go/internal/mod/semver"
	"cuelang.org/go/mod/modfile"
)

// ValidatePublishTag check for semver compatible from publish tag
// accept special latest tag
// which will be transformed into empty tag in order to be compatible with cue internal
func ValidatePublishTag(v string, mf *modfile.File, modPath string) (string, error) {
	// accept special latest version
	if v == "latest" {
		return "", nil
	}

	// START original validation logic
	if !semver.IsValid(v) {
		return "", fmt.Errorf("invalid publish version %q; must be valid semantic version (see http://semver.org)", v)
	}
	if semver.Canonical(v) != v {
		return "", fmt.Errorf("publish version %q is not in canonical form", v)
	}
	if major := mf.MajorVersion(); semver.Major(v) != major {
		if _, _, ok := ast.SplitPackageVersion(mf.Module); ok {
			return "", fmt.Errorf("publish version %q does not match the major version %q declared in %q; must be %s.N.N", v, major, modPath, major)
		} else {
			return "", fmt.Errorf("publish version %q does not match implied major version %q in %q; must be %s.N.N", v, major, modPath, major)
		}
	}
	// END original validation logic

	return v, nil
}
