package footprint

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spore-host/libs/tags"
)

// NamePattern is one of a tool's registered resource names: resources of
// Type (or of any type, if Type is empty) whose Name matches Pattern in full.
type NamePattern struct {
	Type    string
	Pattern *regexp.Regexp
}

// NewPattern compiles expr as a whole-name match (it is anchored at both
// ends) for resources of type typ ("" for any type).
func NewPattern(typ, expr string) (NamePattern, error) {
	re, err := regexp.Compile(`^(?:` + expr + `)$`)
	if err != nil {
		return NamePattern{}, fmt.Errorf("footprint: name pattern %q: %w", expr, err)
	}
	return NamePattern{Type: typ, Pattern: re}, nil
}

// MustPattern is NewPattern for patterns fixed at compile time; it panics on
// a bad expression.
func MustPattern(typ, expr string) NamePattern {
	p, err := NewPattern(typ, expr)
	if err != nil {
		panic(err)
	}
	return p
}

// Matches reports whether r's type and name match p.
func (p NamePattern) Matches(r Resource) bool {
	return p.Pattern != nil && (p.Type == "" || p.Type == r.Type) && p.Pattern.MatchString(r.Name)
}

// BoundaryPolicyName is the permission boundary that `spawn setup-account`
// installs (path /spore-host/). It is the one piece of spore.host meant to
// outlive every teardown, and only `spawn setup-account --remove` deletes it.
const BoundaryPolicyName = "spawn-instance-boundary"

// IsBoundary reports whether r is the spore-host/spawn-instance-boundary
// policy, by ARN or by type and name.
func IsBoundary(r Resource) bool {
	return strings.HasSuffix(r.ARN, ":policy/spore-host/"+BoundaryPolicyName) ||
		(r.Type == "AWS::IAM::ManagedPolicy" && r.Name == BoundaryPolicyName)
}

// Deletable is the delete guard. It reports true only if every one of these
// holds, and otherwise the first that fails, as the reason:
//
//   - its ownership is tagged (a legacy-name match is never deleted);
//   - it is tagged spore-host:managed-by=<tool>;
//   - it is not part of the tool's service machinery (role=service);
//   - it is not tagged spore-host:permanent=true, and is not the account's
//     spawn-instance-boundary policy;
//   - its class is litter, residue or active (not user-data, not permanent,
//     and not unset or unknown);
//   - its name matches one of patterns.
func Deletable(r Resource, tool tags.Tool, patterns []NamePattern) (bool, string) {
	switch {
	case tool == "":
		return false, "no tool given"
	case r.Ownership != OwnershipTagged:
		return false, fmt.Sprintf("ownership is %q: only resources tagged %s=%s are deleted; name matches are reported only", r.Ownership, tags.ManagedBy, tool)
	case !tags.IsManagedBy(r.Tags, tool):
		return false, fmt.Sprintf("not tagged %s=%s", tags.ManagedBy, tool)
	case tags.IsService(r.Tags):
		return false, fmt.Sprintf("part of %s's own service (%s=%s)", tool, tags.Role, tags.RoleService)
	case tags.IsPermanent(r.Tags):
		return false, fmt.Sprintf("tagged %s=%s", tags.Permanent, tags.PermanentTrue)
	case IsBoundary(r):
		return false, "the account's " + BoundaryPolicyName + " policy is permanent"
	}
	switch r.Class {
	case ClassLitter, ClassResidue, ClassActive:
	case ClassUserData, ClassPermanent:
		return false, fmt.Sprintf("class %s is never deleted by teardown", r.Class)
	default:
		return false, fmt.Sprintf("class %q is not a known class", r.Class)
	}
	for _, p := range patterns {
		if p.Matches(r) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("name %q matches none of %s's patterns for %s", r.Name, tool, r.Type)
}
