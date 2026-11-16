// Package footprint is the shared inventory schema and teardown executor for
// the spore.host tools' footprint commands (`<tool> footprint -o json`) and
// their idle teardown.
//
// It is part of leave-no-trace (spore-host/spore-host#752). Each tool lists
// what it has in an account as a Report of Resources, classifies each one,
// and decides with Deletable whether it may be removed. Teardown then builds
// a Plan, which orders the resources so that dependents are deleted before
// what they depend on, and runs it with Execute, in Report mode (say what
// would be deleted) or Reap mode (delete it). Before every delete, Execute
// re-reads the resource and re-applies the guard to its live tags, and
// heartbeats the teardown lease (package lifecycle), so a resource that
// changed hands, or a lease that was lost, stops the deletion.
//
// The guard is deliberately narrow. A resource is deletable only if it is
// tagged managed-by=<tool>, its name matches one of the tool's patterns, it
// is not part of the tool's service machinery or marked permanent, and it is
// not user data. Resources matched by name alone (ownership legacy-name) are
// reported, never deleted.
package footprint

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spore-host/libs/tags"
)

// SchemaVersion is the version of the Report JSON schema. It changes only when
// a field is removed or its meaning changes; adding a field does not change it.
const SchemaVersion = 1

// Ownership says how a resource was attributed to the tool.
type Ownership string

// Ownership values.
const (
	// OwnershipTagged: the resource carries spore-host:managed-by=<tool>.
	OwnershipTagged Ownership = "tagged"
	// OwnershipLegacyName: only its name matches the tool's patterns (it
	// predates the tags). Reported, never deleted.
	OwnershipLegacyName Ownership = "legacy-name"
	// OwnershipForeign: it belongs to something else (another tool, the
	// user), listed because the tool depends on it.
	OwnershipForeign Ownership = "foreign"
)

// Valid reports whether o is one of the defined values.
func (o Ownership) Valid() bool {
	switch o {
	case OwnershipTagged, OwnershipLegacyName, OwnershipForeign:
		return true
	}
	return false
}

// Class says what a resource is, for the purpose of removing it.
type Class string

// Class values.
const (
	// ClassLitter: left behind by a finished or failed run; removable now.
	ClassLitter Class = "litter"
	// ClassUserData: holds the user's data (results, history). Never deleted
	// by teardown; the user exports or removes it.
	ClassUserData Class = "user-data"
	// ClassPermanent: must outlive every teardown (the account boundary).
	ClassPermanent Class = "permanent"
	// ClassResidue: the tool's own machinery, removable once the tool is idle.
	ClassResidue Class = "residue"
	// ClassActive: in use right now.
	ClassActive Class = "active"
)

// Valid reports whether c is one of the defined values.
func (c Class) Valid() bool {
	switch c {
	case ClassLitter, ClassUserData, ClassPermanent, ClassResidue, ClassActive:
		return true
	}
	return false
}

// Report is a tool's footprint in one account: the output of
// `<tool> footprint -o json`.
type Report struct {
	Schema      int       `json:"schema"`
	Tool        tags.Tool `json:"tool"`
	Account     string    `json:"account"`
	GeneratedAt time.Time `json:"generated_at"`
	// Resources is JSON "resources", always an array.
	Resources []Resource `json:"-"`
	// Warnings are what the inventory could not check (a listing that
	// failed, a region it could not reach). JSON "warnings", always an array.
	Warnings []string `json:"-"`
}

// Resource is one AWS resource in a footprint.
type Resource struct {
	// ARN identifies the resource; for a resource type without ARNs, use a
	// stable unique identifier.
	ARN string `json:"arn"`
	// Type is the resource type, as a CloudFormation type name
	// (AWS::IAM::Role, AWS::Lambda::Function, ...).
	Type string `json:"type"`
	// Region is the resource's region, or "global" for IAM and other
	// account-wide resources.
	Region string `json:"region"`
	// Scope is the spore-host:scope tag value, if any.
	Scope string `json:"scope"`
	// Name is the resource's name, which the tool's NamePatterns match.
	Name      string    `json:"name"`
	Ownership Ownership `json:"ownership"`
	Class     Class     `json:"class"`
	// Deletable and Reason are Deletable's verdict, as of when the report
	// was made.
	Deletable bool   `json:"deletable"`
	Reason    string `json:"reason"`
	// DependsOn lists the ARNs of resources this one depends on: it must be
	// deleted before them. JSON "depends_on", always an array.
	DependsOn []string `json:"-"`
	// Tags are the resource's tags. JSON "tags", always an object.
	Tags map[string]string `json:"-"`
}

// NewReport returns an empty report for tool in account, stamped now.
func NewReport(tool tags.Tool, account string, now time.Time) *Report {
	return &Report{Schema: SchemaVersion, Tool: tool, Account: account, GeneratedAt: now.UTC()}
}

// MarshalJSON writes resources and warnings as arrays even when empty, so
// consumers never see null.
func (r Report) MarshalJSON() ([]byte, error) {
	type wire struct {
		Schema      int        `json:"schema"`
		Tool        tags.Tool  `json:"tool"`
		Account     string     `json:"account"`
		GeneratedAt time.Time  `json:"generated_at"`
		Resources   []Resource `json:"resources"`
		Warnings    []string   `json:"warnings"`
	}
	w := wire(r)
	if w.Resources == nil {
		w.Resources = []Resource{}
	}
	if w.Warnings == nil {
		w.Warnings = []string{}
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads the wire form MarshalJSON writes.
func (r *Report) UnmarshalJSON(b []byte) error {
	var w struct {
		Schema      int        `json:"schema"`
		Tool        tags.Tool  `json:"tool"`
		Account     string     `json:"account"`
		GeneratedAt time.Time  `json:"generated_at"`
		Resources   []Resource `json:"resources"`
		Warnings    []string   `json:"warnings"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*r = Report(w)
	return nil
}

// MarshalJSON writes depends_on and tags as an array and an object even when
// empty.
func (r Resource) MarshalJSON() ([]byte, error) {
	type plain Resource // no methods, so no recursion; its json:"-" fields are dropped
	w := struct {
		plain
		DependsOn []string          `json:"depends_on"`
		Tags      map[string]string `json:"tags"`
	}{plain: plain(r), DependsOn: r.DependsOn, Tags: r.Tags}
	if w.DependsOn == nil {
		w.DependsOn = []string{}
	}
	if w.Tags == nil {
		w.Tags = map[string]string{}
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads the wire form MarshalJSON writes.
func (r *Resource) UnmarshalJSON(b []byte) error {
	type plain Resource
	var w struct {
		plain
		DependsOn []string          `json:"depends_on"`
		Tags      map[string]string `json:"tags"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*r = Resource(w.plain)
	r.DependsOn, r.Tags = w.DependsOn, w.Tags
	return nil
}

// Decode reads a Report and checks it: its schema version must be one this
// package understands, and every resource's ownership and class must be
// defined values.
func Decode(rd io.Reader) (*Report, error) {
	var r Report
	if err := json.NewDecoder(rd).Decode(&r); err != nil {
		return nil, fmt.Errorf("footprint: decode report: %w", err)
	}
	if r.Schema != SchemaVersion {
		return nil, fmt.Errorf("footprint: report schema %d, this build reads %d", r.Schema, SchemaVersion)
	}
	for i, res := range r.Resources {
		if !res.Ownership.Valid() || !res.Class.Valid() {
			return nil, fmt.Errorf("footprint: resource %d (%s): ownership %q, class %q", i, res.ARN, res.Ownership, res.Class)
		}
	}
	return &r, nil
}
