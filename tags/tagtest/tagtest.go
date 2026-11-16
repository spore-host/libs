// Package tagtest fails a unit test when code under test creates an AWS resource
// without the spore-host:managed-by tag (see package tags).
//
// A resource created untagged is invisible to leave-no-trace cleanup
// (spore-host/spore-host#752): nothing will ever select it for removal, so it
// leaks. This package catches that in unit tests, before anything reaches AWS.
//
// # How it works
//
// A Detector is an AWS SDK v2 middleware. Install it on the aws.Config (or a
// single client's options) used by the code under test. On every operation whose
// name marks it as creating something (see IsCreate), it inspects the input by
// reflection for a managed-by tag naming the expected tool, and calls tb.Errorf
// when there isn't one. It does not stop the call, so the test's own fake
// transport or stub still sees it, and every untagged call in a test is
// reported, not only the first.
//
// It runs at the very start of the operation, before input validation and
// before anything is sent, so it works with any stubbed transport (or none: a
// call that later fails to connect has already been checked).
//
//	func TestLaunch(t *testing.T) {
//		cfg := aws.Config{Region: "us-east-1", HTTPClient: stub}
//		tagtest.New(t, tags.ToolSpawn).Install(&cfg)
//		client := ec2.NewFromConfig(cfg)
//		// ... exercise code that calls client.RunInstances etc.
//	}
//
// For one client only:
//
//	d := tagtest.New(t, tags.ToolSpawn)
//	client := ec2.NewFromConfig(cfg, func(o *ec2.Options) {
//		o.APIOptions = append(o.APIOptions, d.APIOption)
//	})
//
// # What counts as tagged
//
// The input's top-level fields named Tags, TagSpecifications, TagSet, TagList,
// and Tagging are read, in any of the shapes the SDK uses: a map[string]string
// (SQS, Lambda, CloudWatch Logs), a slice of {Key, Value} structs (EC2, IAM,
// DynamoDB, SNS, Scheduler, ...), a struct holding a TagSet (S3 *types.Tagging),
// a URL query string (S3 PutObject's Tagging), and EC2 TagSpecifications. S3
// CreateBucket's CreateBucketConfiguration.Tags is read too.
//
// The input passes when one of those carries managed-by=<tool>. It fails when
// none does, when managed-by names a different tool, or when any one of several
// EC2 TagSpecifications lacks it (a RunInstances that tags the instance but not
// its volumes leaks the volumes).
//
// # The allow-list
//
// Some matching operations do not create a taggable resource (PutRolePolicy,
// DynamoDB PutItem, EventBridge Scheduler CreateSchedule, ...). DefaultAllowList
// covers the common ones, each with the reason it is safe. Pass further Allow
// entries to New; each needs a Reason, which is the documentation for why that
// call may go out untagged. A typical caller entry is for a flow that tags a
// resource in a follow-up call:
//
//	tagtest.New(t, tags.ToolSpawn, tagtest.Allow{
//		Service: "S3", Operation: "CreateBucket",
//		Reason: "results buckets are tagged by the PutBucketTagging that follows",
//	})
//
// Prefer tagging at create where the API allows it: there is then no window in
// which a crash leaves the resource untagged.
package tagtest

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"

	"github.com/spore-host/libs/tags"
)

// MiddlewareID is the ID the Detector registers in the SDK middleware stack.
const MiddlewareID = "spore-host/tagtest"

// createPrefixes are the operation-name prefixes that mark an operation as one
// that may create a resource.
var createPrefixes = []string{
	"Create", "Put", "Run", "Register", "Import", "Allocate",
	"CopyImage", "CopySnapshot", "RequestSpotInstances", "RequestSpotFleet",
}

// IsCreate reports whether the Detector checks an operation with this name:
// one starting with Create, Put, Run, Register, Import, or Allocate, or one of
// CopyImage, CopySnapshot, RequestSpotInstances, and RequestSpotFleet.
func IsCreate(operation string) bool {
	for _, p := range createPrefixes {
		if strings.HasPrefix(operation, p) {
			return true
		}
	}
	return false
}

// Allow exempts one operation from the check.
type Allow struct {
	// Service is the SDK service ID, as in the service package's ServiceID
	// constant: "EC2", "IAM", "S3", "DynamoDB", "Scheduler", "SQS", "SNS",
	// "Lambda", "CloudWatch Logs", "CloudWatch", "EventBridge", ...
	Service string
	// Operation is the operation name, for example "PutRolePolicy".
	Operation string
	// Reason says why the operation may go out without a managed-by tag.
	// It is required.
	Reason string
}

const (
	reasonTagsCall      = "tags an existing resource; that resource's own create call is what is checked"
	reasonBucketConf    = "bucket configuration, not a resource; the bucket is checked at CreateBucket"
	reasonPartOfRole    = "an attribute of an IAM role, not a resource; the role is checked at CreateRole"
	reasonEventsAreData = "writes data, not a resource"
)

var defaultAllowList = []Allow{
	{"EC2", "CreateTags", reasonTagsCall},
	{"EC2", "CreateRoute", "a route is part of its route table and cannot be tagged"},
	{"EC2", "CreateLaunchTemplateVersion", "a version belongs to its launch template, which is checked at CreateLaunchTemplate"},

	{"IAM", "PutRolePolicy", "an inline policy is part of its role and cannot be tagged; the role is checked at CreateRole"},
	{"IAM", "PutRolePermissionsBoundary", reasonPartOfRole},
	{"IAM", "CreatePolicyVersion", "a version belongs to its managed policy, which is checked at CreatePolicy"},
	{"IAM", "CreateServiceLinkedRole", "takes no tags; a service-linked role is owned and removed by its AWS service"},

	{"S3", "PutBucketPolicy", reasonBucketConf},
	{"S3", "PutBucketLifecycleConfiguration", reasonBucketConf},
	{"S3", "PutBucketVersioning", reasonBucketConf},
	{"S3", "PutBucketEncryption", reasonBucketConf},
	{"S3", "PutPublicAccessBlock", reasonBucketConf},
	{"S3", "PutBucketOwnershipControls", reasonBucketConf},
	{"S3", "PutBucketNotificationConfiguration", reasonBucketConf},
	{"S3", "PutBucketCors", reasonBucketConf},

	{"DynamoDB", "PutItem", "an item is data in a table, not a resource; the table is checked at CreateTable"},

	{"Scheduler", "CreateSchedule", "a schedule cannot be tagged; its schedule group is checked at CreateScheduleGroup"},

	{"CloudWatch Logs", "CreateLogStream", "a log stream cannot be tagged; its log group is checked at CreateLogGroup"},
	{"CloudWatch Logs", "PutLogEvents", reasonEventsAreData},
	{"CloudWatch Logs", "PutRetentionPolicy", "a log group setting, not a resource"},
	{"CloudWatch Logs", "PutSubscriptionFilter", "part of its log group and cannot be tagged"},

	{"CloudWatch", "PutMetricData", reasonEventsAreData},

	{"EventBridge", "PutEvents", reasonEventsAreData},
	{"EventBridge", "PutTargets", "targets are part of their rule, which is checked at PutRule"},
	{"EventBridge", "PutPermission", "an event bus policy statement, not a resource"},

	{"Lambda", "CreateAlias", "an alias belongs to its function, which is checked at CreateFunction"},
	{"Lambda", "PutFunctionConcurrency", "a function setting, not a resource"},
}

// DefaultAllowList returns the operations every Detector exempts, each with its
// reason. The result is a copy.
func DefaultAllowList() []Allow {
	return append([]Allow(nil), defaultAllowList...)
}

// Call is one create operation the Detector saw.
type Call struct {
	Service   string
	Operation string
	// Tagged is true when the input carried managed-by=<tool>.
	Tagged bool
	// Allowed is true when the operation was on the allow-list. An allowed
	// call is not inspected, so Tagged is false for it.
	Allowed bool
	// Reason is the allow-list reason for an allowed call, and what was wrong
	// for a call that failed the check. It is empty for a tagged call.
	Reason string
}

// Detector checks create calls for a managed-by tag. Create one per test with
// New. It is safe for concurrent use.
type Detector struct {
	tb    testing.TB
	tool  tags.Tool
	allow map[string]string // "Service/Operation" -> reason

	mu    sync.Mutex
	calls []Call
}

// New returns a Detector that fails tb when a create call lacks
// managed-by=<tool>. The allow entries are added to DefaultAllowList. An entry
// with an empty Service, Operation, or Reason fails tb at once.
func New(tb testing.TB, tool tags.Tool, allow ...Allow) *Detector {
	tb.Helper()
	d := &Detector{tb: tb, tool: tool, allow: make(map[string]string)}
	for _, a := range append(DefaultAllowList(), allow...) {
		if a.Service == "" || a.Operation == "" || strings.TrimSpace(a.Reason) == "" {
			tb.Fatalf("tagtest: allow-list entry %+v needs a Service, an Operation, and a Reason", a)
		}
		d.allow[a.Service+"/"+a.Operation] = a.Reason
	}
	return d
}

// Install adds the Detector to every client built from cfg.
func (d *Detector) Install(cfg *aws.Config) {
	cfg.APIOptions = append(cfg.APIOptions, d.APIOption)
}

// APIOption adds the Detector to a middleware stack. It has the signature of
// an aws.Config.APIOptions (or a client Options.APIOptions) element. Adding it
// twice to the same client (for example through both the config and the client
// options) is an error from the SDK, because the middleware ID is taken.
func (d *Detector) APIOption(stack *middleware.Stack) error {
	m := middleware.InitializeMiddlewareFunc(MiddlewareID, d.handle)
	// Run after the SDK records the service and operation names, when that
	// middleware is present, and in any case before input validation.
	if _, ok := stack.Initialize.Get("RegisterServiceMetadata"); ok {
		return stack.Initialize.Insert(m, "RegisterServiceMetadata", middleware.After)
	}
	return stack.Initialize.Add(m, middleware.Before)
}

// Calls returns every create call seen so far, in order, including allowed ones.
func (d *Detector) Calls() []Call {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Call(nil), d.calls...)
}

func (d *Detector) handle(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
	service, op := middleware.GetServiceID(ctx), middleware.GetOperationName(ctx)
	if service == "" {
		service = awsmiddleware.GetServiceID(ctx)
	}
	if op == "" {
		op = awsmiddleware.GetOperationName(ctx)
	}
	if IsCreate(op) {
		d.check(service, op, in.Parameters)
	}
	return next.HandleInitialize(ctx, in)
}

func (d *Detector) check(service, op string, params any) {
	c := Call{Service: service, Operation: op}
	if reason, ok := d.allow[service+"/"+op]; ok {
		c.Allowed, c.Reason = true, reason
	} else {
		c.Tagged, c.Reason = inspect(service, op, params, d.tool)
	}
	d.mu.Lock()
	d.calls = append(d.calls, c)
	d.mu.Unlock()
	if !c.Tagged && !c.Allowed {
		d.tb.Errorf("tagtest: %s %s would create a resource without %s=%s: %s.\n"+
			"Tag it at create (tags.For), or, if it cannot carry tags, allow-list it: "+
			"tagtest.Allow{Service: %q, Operation: %q, Reason: \"...\"}",
			service, op, tags.ManagedBy, d.tool, c.Reason, service, op)
	}
}

// tagFields are the top-level input fields that carry tags.
var tagFields = []string{"Tags", "TagSpecifications", "TagSet", "TagList", "Tagging"}

// nestedTagFields are inputs whose tags sit one struct down.
var nestedTagFields = map[string]string{
	"S3/CreateBucket": "CreateBucketConfiguration",
}

// inspect reports whether params carries managed-by=tool, and if not, why.
func inspect(service, op string, params any, tool tags.Tool) (bool, string) {
	v, ok := deref(reflect.ValueOf(params))
	if !ok || v.Kind() != reflect.Struct {
		return false, fmt.Sprintf("input %T is not a struct", params)
	}
	type scope struct {
		prefix string
		v      reflect.Value
	}
	scopes := []scope{{"", v}}
	var seen, problems []string
	if name, ok := nestedTagFields[service+"/"+op]; ok {
		// nosemgrep: go.lang.security.audit.unsafe-reflect-by-name.unsafe-reflect-by-name -- name is from the fixed nestedTagFields table, not from input
		nf := v.FieldByName(name)
		if nv, ok := deref(nf); ok && nv.Kind() == reflect.Struct {
			scopes = append(scopes, scope{name + ".", nv})
		} else if nf.IsValid() {
			seen = append(seen, name+".Tags") // nil: a tag field, but empty
		}
	}

	tagged := false
	for _, sc := range scopes {
		for _, name := range tagFields {
			// nosemgrep: go.lang.security.audit.unsafe-reflect-by-name.unsafe-reflect-by-name -- name is from the fixed tagFields list, not from input
			f := sc.v.FieldByName(name)
			if !f.IsValid() {
				continue
			}
			label := sc.prefix + name
			seen = append(seen, label)
			var ok bool
			var problem string
			if name == "TagSpecifications" {
				ok, problem = checkSpecs(f, tool)
			} else {
				ok, problem = checkTags(label, f, tool)
			}
			if ok {
				tagged = true
			} else if problem != "" {
				problems = append(problems, problem)
			}
		}
	}
	switch {
	case len(seen) == 0:
		return false, fmt.Sprintf("input %T has no tag field (%s)", params, strings.Join(tagFields, ", "))
	case tagged && len(problems) == 0:
		return true, ""
	case len(problems) > 0:
		return false, strings.Join(problems, "; ")
	default:
		return false, fmt.Sprintf("no %s tag in %s", tags.ManagedBy, strings.Join(seen, ", "))
	}
}

// checkSpecs requires every EC2 TagSpecification to carry managed-by=tool.
func checkSpecs(f reflect.Value, tool tags.Tool) (bool, string) {
	f, ok := deref(f)
	if !ok || f.Kind() != reflect.Slice || f.Len() == 0 {
		return false, ""
	}
	var problems []string
	for i := 0; i < f.Len(); i++ {
		spec, ok := deref(f.Index(i))
		if !ok || spec.Kind() != reflect.Struct {
			continue
		}
		label := fmt.Sprintf("TagSpecifications[%d]", i)
		if rt := spec.FieldByName("ResourceType"); rt.IsValid() && rt.Kind() == reflect.String && rt.String() != "" {
			label += " (" + rt.String() + ")"
		}
		ok, problem := checkTags(label, spec.FieldByName("Tags"), tool)
		if !ok {
			if problem == "" {
				problem = label + " has no " + tags.ManagedBy + " tag"
			}
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		return false, strings.Join(problems, "; ")
	}
	return true, ""
}

// checkTags reads one tag-bearing field. It reports ok when managed-by=tool is
// present, and a problem when managed-by names another tool or the field's
// shape is not one it knows. A field with no managed-by tag yields neither.
func checkTags(label string, f reflect.Value, tool tags.Tool) (bool, string) {
	m, known := toMap(f)
	if !known {
		return false, fmt.Sprintf("%s has a shape tagtest cannot read (%s)", label, f.Type())
	}
	v, ok := m[tags.ManagedBy]
	switch {
	case !ok:
		return false, ""
	case v != string(tool):
		return false, fmt.Sprintf("%s has %s=%s, want %s", label, tags.ManagedBy, v, tool)
	default:
		return true, ""
	}
}

// toMap reads tags from the shapes the SDK uses. It reports false for a shape
// it does not recognise.
func toMap(f reflect.Value) (map[string]string, bool) {
	if !f.IsValid() {
		return nil, true
	}
	v, ok := deref(f)
	if !ok {
		return nil, true // nil pointer: no tags
	}
	switch v.Kind() {
	case reflect.String: // S3 Tagging: "k1=v1&k2=v2"
		q, err := url.ParseQuery(v.String())
		if err != nil {
			return nil, true
		}
		m := make(map[string]string, len(q))
		for k, vs := range q {
			if len(vs) > 0 {
				m[k] = vs[0]
			}
		}
		return m, true
	case reflect.Map: // map[string]string, map[string]*string
		if v.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		m := make(map[string]string, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			s, ok := str(iter.Value())
			if !ok {
				return nil, false
			}
			m[iter.Key().String()] = s
		}
		return m, true
	case reflect.Slice: // []struct{Key, Value}
		m := make(map[string]string, v.Len())
		for i := 0; i < v.Len(); i++ {
			e, ok := deref(v.Index(i))
			if !ok {
				continue
			}
			if e.Kind() != reflect.Struct {
				return nil, false
			}
			kf, vf := e.FieldByName("Key"), e.FieldByName("Value")
			if !kf.IsValid() || !vf.IsValid() {
				return nil, false
			}
			k, kok := str(kf)
			val, vok := str(vf)
			if !kok || !vok {
				return nil, false
			}
			m[k] = val
		}
		return m, true
	case reflect.Struct: // S3 *types.Tagging{TagSet}
		if ts := v.FieldByName("TagSet"); ts.IsValid() {
			return toMap(ts)
		}
		if ts := v.FieldByName("Tags"); ts.IsValid() {
			return toMap(ts)
		}
		return nil, false
	}
	return nil, false
}

// deref follows pointers and interfaces. It reports false for a nil one.
func deref(v reflect.Value) (reflect.Value, bool) {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	return v, v.IsValid()
}

// str reads a string or *string (nil reads as "").
func str(v reflect.Value) (string, bool) {
	d, ok := deref(v)
	if !ok {
		return "", v.Kind() == reflect.Pointer
	}
	if d.Kind() != reflect.String {
		return "", false
	}
	return d.String(), true
}
