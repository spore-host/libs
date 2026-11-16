// Package tags defines the spore-host:* resource tags that every spore.host tool
// (spawn, lagotto, calque, truffle, spore-host-mcp) puts on the AWS resources it
// creates, so that each tool can later find, and remove, exactly what it made.
//
// It is part of the leave-no-trace design (spore-host/spore-host#752): a tool
// removes what it created once nothing it manages is active, after a grace
// period. That only works if every created resource says who made it and when,
// which is what these tags record.
//
// This package is dependency-free. The tag sets are plain maps, which is the
// shape SQS (CreateQueue), Lambda (CreateFunction), and CloudWatch Logs
// (CreateLogGroup) take directly. For services that take a slice of tag structs,
// use the per-service converter subpackages, each of which imports only that
// service's SDK types package:
//
//	tags/ec2tags        EC2 []types.Tag and []types.TagSpecification
//	tags/iamtags        IAM []types.Tag
//	tags/dynamodbtags   DynamoDB []types.Tag
//	tags/snstags        SNS []types.Tag
//	tags/s3tags         S3 []types.Tag, *types.Tagging, and the PutObject query string
//	tags/schedulertags  EventBridge Scheduler []types.Tag (schedule groups)
//
// Unit tests can check that no create call goes out untagged with tags/tagtest.
//
// Typical use:
//
//	t := tags.For(tags.ToolSpawn, scope, time.Now(), owner)
//	_, err := sqsClient.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: &name, Tags: t})
//	_, err = ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
//		GroupName:         &name,
//		Description:       &desc,
//		TagSpecifications: ec2tags.Specifications(t, ec2types.ResourceTypeSecurityGroup),
//	})
//
// A long-lived resource that a tool runs on (a reaper Lambda, its role, its
// schedule group) additionally carries Role=RoleService, so cleanup can tell the
// tool's own machinery from the resources it manages:
//
//	t := tags.For(tags.ToolSpawn, scope, time.Now(), owner)
//	t[tags.Role] = tags.RoleService
//	t[tags.Component] = "reaper"
package tags

import "time"

// Tag keys. All live under the spore-host: prefix, which AWS does not reserve
// (only aws: is reserved) and which every service used by spore.host accepts.
const (
	// ManagedBy names the tool that created the resource and is responsible for
	// removing it. The value is a Tool. This is the key cleanup selects on.
	ManagedBy = "spore-host:managed-by"

	// Scope says which run, sweep, watch, or deployment the resource belongs to.
	// Its value is opaque to this package; each tool defines its own.
	Scope = "spore-host:scope"

	// Created is when the resource was created, in RFC 3339 UTC to the second
	// (for example 2026-11-16T10:00:00Z). Cleanup's grace period counts from it.
	Created = "spore-host:created"

	// Schema is the version of this tag scheme. Its value is SchemaVersion.
	Schema = "spore-host:schema"

	// Owner identifies who the resource was created for (for example a user or
	// team name). Its value is opaque to this package.
	Owner = "spore-host:owner"

	// Role marks what part a resource plays. The only defined value is
	// RoleService. A resource without it is something the tool manages.
	Role = "spore-host:role"

	// Component names the part of a tool a resource belongs to (for example
	// "reaper"). Its value is opaque to this package.
	Component = "spore-host:component"
)

// Tag values with a fixed meaning.
const (
	// SchemaVersion is the value of the Schema tag for this tag scheme.
	SchemaVersion = "1"

	// RoleService is the value of the Role tag on a resource that is part of a
	// tool's own long-lived machinery (a reaper Lambda, its IAM role, its
	// schedule group), as opposed to a resource the tool manages for a user.
	RoleService = "service"
)

// Tool is a spore.host tool name, used as the value of the ManagedBy tag.
// It is a distinct type so that a tool name and a scope (both strings) cannot
// be swapped silently in a call to For.
type Tool string

// The spore.host tools. These values are written to AWS and read back by
// cleanup, so they never change.
const (
	ToolSpawn   Tool = "spawn"
	ToolLagotto Tool = "lagotto"
	ToolCalque  Tool = "calque"
	ToolTruffle Tool = "truffle"
	ToolMCP     Tool = "mcp"
)

// For returns the standard tag set for a resource that tool creates now:
// ManagedBy, Scope, Created, Schema, and Owner. Scope and Owner are left out
// when empty, since an empty value records nothing. A zero now is replaced by
// the current time: a resource stamped with year 1 would look ancient, and so
// immediately eligible for cleanup.
//
// The result is a new map that the caller owns and may add to (Role, Component,
// Name, ...). It is the tag shape SQS, Lambda, and CloudWatch Logs take
// directly; the converter subpackages turn it into the other services' shapes.
func For(tool Tool, scope string, now time.Time, owner string) map[string]string {
	if now.IsZero() {
		now = time.Now()
	}
	m := map[string]string{
		ManagedBy: string(tool),
		Created:   FormatCreated(now),
		Schema:    SchemaVersion,
	}
	if scope != "" {
		m[Scope] = scope
	}
	if owner != "" {
		m[Owner] = owner
	}
	return m
}

// FormatCreated formats t as the value of the Created tag: RFC 3339 in UTC,
// truncated to the second.
func FormatCreated(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// CreatedAt parses the Created tag. It reports false when the tag is missing or
// is not RFC 3339, so a caller can treat such a resource as having unknown age
// rather than guessing.
func CreatedAt(tags map[string]string) (time.Time, bool) {
	v, ok := tags[Created]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// IsManagedBy reports whether tags say the resource was created by tool.
// A nil map is not managed by anything.
func IsManagedBy(tags map[string]string, tool Tool) bool {
	v, ok := tags[ManagedBy]
	return ok && v == string(tool)
}

// IsService reports whether tags mark the resource as part of a tool's own
// long-lived machinery (Role=RoleService).
func IsService(tags map[string]string) bool {
	return tags[Role] == RoleService
}
