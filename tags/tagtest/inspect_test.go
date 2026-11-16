package tagtest

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"

	"github.com/spore-host/libs/tags"
)

func TestInspectShapes(t *testing.T) {
	mb := tags.ManagedBy
	spawn := "spawn"
	type tagKV struct{ Key, Value string }
	type tagPtr struct{ Key, Value *string }
	for _, tc := range []struct {
		name    string
		in      any
		ok      bool
		wantWhy string
	}{
		{"map[string]string", &struct{ Tags map[string]string }{map[string]string{mb: "spawn"}}, true, ""},
		{"map[string]*string", &struct{ Tags map[string]*string }{map[string]*string{mb: &spawn}}, true, ""},
		{"[]struct{string}", &struct{ Tags []tagKV }{[]tagKV{{mb, "spawn"}}}, true, ""},
		{"[]*struct{*string}", &struct{ TagList []*tagPtr }{[]*tagPtr{nil, {&mb, &spawn}}}, true, ""},
		{"TagSet field", &struct{ TagSet []tagKV }{[]tagKV{{mb, "spawn"}}}, true, ""},
		{"query string", struct{ Tagging string }{"a=b&" + mb + "=spawn"}, true, ""},
		{"empty map", &struct{ Tags map[string]string }{}, false, "no " + mb + " tag in Tags"},
		{"other tool", &struct{ Tags []tagKV }{[]tagKV{{mb, "calque"}}}, false, "=calque, want spawn"},
		{"unknown map value", &struct{ Tags map[string]int }{map[string]int{mb: 1}}, false, "cannot read"},
		{"unknown element", &struct{ Tags []string }{[]string{mb}}, false, "cannot read"},
		{"element without Value", &struct{ Tags []struct{ Key string } }{[]struct{ Key string }{{mb}}}, false, "cannot read"},
		{"non-string key", &struct{ Tags map[int]string }{map[int]string{1: "x"}}, false, "cannot read"},
		{"number field", &struct{ Tags int }{1}, false, "cannot read"},
		{"struct without TagSet", &struct{ Tagging struct{ X int } }{}, false, "cannot read"},
		{"bad query", struct{ Tagging string }{"%zz"}, false, "no " + mb},
		{"no tag field", &struct{ Name string }{"x"}, false, "has no tag field"},
		{"nil input", nil, false, "not a struct"},
		{"non-struct input", "x", false, "not a struct"},
		{"empty TagSpecifications", &ec2.RunInstancesInput{}, false, "no " + mb + " tag in TagSpecifications"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := inspect("X", "CreateX", tc.in, tags.ToolSpawn)
			if ok != tc.ok || !strings.Contains(why, tc.wantWhy) {
				t.Errorf("inspect = %v, %q; want %v, containing %q", ok, why, tc.ok, tc.wantWhy)
			}
		})
	}
}

// Every default allow-list entry this module can check against the SDK must
// really be an input that cannot carry tags (or, for CreateTags, one that tags
// something else). Entries for services not in go.mod (CloudWatch Logs,
// CloudWatch, EventBridge, Lambda) were checked by hand against their SDK
// inputs.
func TestDefaultAllowListEntriesHaveNoTagField(t *testing.T) {
	inputs := map[string]any{
		"EC2/CreateRoute":                       ec2.CreateRouteInput{},
		"EC2/CreateLaunchTemplateVersion":       ec2.CreateLaunchTemplateVersionInput{},
		"IAM/PutRolePolicy":                     iam.PutRolePolicyInput{},
		"IAM/PutRolePermissionsBoundary":        iam.PutRolePermissionsBoundaryInput{},
		"IAM/CreatePolicyVersion":               iam.CreatePolicyVersionInput{},
		"IAM/CreateServiceLinkedRole":           iam.CreateServiceLinkedRoleInput{},
		"S3/PutBucketPolicy":                    s3.PutBucketPolicyInput{},
		"S3/PutBucketLifecycleConfiguration":    s3.PutBucketLifecycleConfigurationInput{},
		"S3/PutBucketVersioning":                s3.PutBucketVersioningInput{},
		"S3/PutBucketEncryption":                s3.PutBucketEncryptionInput{},
		"S3/PutPublicAccessBlock":               s3.PutPublicAccessBlockInput{},
		"S3/PutBucketOwnershipControls":         s3.PutBucketOwnershipControlsInput{},
		"S3/PutBucketNotificationConfiguration": s3.PutBucketNotificationConfigurationInput{},
		"S3/PutBucketCors":                      s3.PutBucketCorsInput{},
		"DynamoDB/PutItem":                      dynamodb.PutItemInput{},
		"Scheduler/CreateSchedule":              scheduler.CreateScheduleInput{},
	}
	checked := 0
	for _, a := range DefaultAllowList() {
		in, ok := inputs[a.Service+"/"+a.Operation]
		if !ok {
			continue
		}
		checked++
		typ := reflect.TypeOf(in)
		for _, f := range tagFields {
			if _, has := typ.FieldByName(f); has {
				t.Errorf("%s/%s is allow-listed but %s has a %s field", a.Service, a.Operation, typ, f)
			}
		}
	}
	if checked != len(inputs) {
		t.Errorf("checked %d of %d entries; an entry was renamed or dropped", checked, len(inputs))
	}
}
