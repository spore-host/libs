package tagtest_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/ec2tags"
	"github.com/spore-host/libs/tags/iamtags"
	"github.com/spore-host/libs/tags/s3tags"
	"github.com/spore-host/libs/tags/snstags"
	"github.com/spore-host/libs/tags/tagtest"
)

// recorder stands in for the test's *testing.T so the Detector's failures can
// be asserted on instead of failing this test.
type recorder struct {
	testing.TB
	mu     sync.Mutex
	errs   []string
	fatals []string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

// stubHTTP is the transport: it records that a request got this far and
// refuses it, so nothing leaves the process.
type stubHTTP struct {
	mu   sync.Mutex
	reqs int
}

var errNoNetwork = errors.New("stub transport: no network in unit tests")

func (s *stubHTTP) Do(*http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.reqs++
	s.mu.Unlock()
	return nil, errNoNetwork
}

// setup returns a config with the Detector installed, the recorder it reports
// to, and the stub transport.
func setup(t *testing.T, allow ...tagtest.Allow) (aws.Config, *recorder, *tagtest.Detector, *stubHTTP) {
	t.Helper()
	rec := &recorder{TB: t}
	stub := &stubHTTP{}
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: aws.AnonymousCredentials{},
		HTTPClient:  stub,
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
	d := tagtest.New(rec, tags.ToolSpawn, allow...)
	d.Install(&cfg)
	return cfg, rec, d, stub
}

var ctx = context.Background()

var spawnTags = tags.For(tags.ToolSpawn, "run-1", time.Unix(1_700_000_000, 0), "alice")

func wantPass(t *testing.T, rec *recorder) {
	t.Helper()
	if len(rec.errs) != 0 {
		t.Errorf("detector failed a tagged or allowed call:\n%s", strings.Join(rec.errs, "\n"))
	}
}

func wantFail(t *testing.T, rec *recorder, substrs ...string) {
	t.Helper()
	if len(rec.errs) != 1 {
		t.Fatalf("detector reported %d failures, want 1: %q", len(rec.errs), rec.errs)
	}
	for _, s := range substrs {
		if !strings.Contains(rec.errs[0], s) {
			t.Errorf("failure %q does not mention %q", rec.errs[0], s)
		}
	}
}

func TestTaggedCreateQueuePasses(t *testing.T) {
	cfg, rec, d, stub := setup(t)
	_, err := sqs.NewFromConfig(cfg).CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("q"), Tags: spawnTags})
	if !errors.Is(err, errNoNetwork) {
		t.Errorf("CreateQueue err = %v, want the stub's error (the call must not be stopped)", err)
	}
	wantPass(t, rec)
	if stub.reqs != 1 {
		t.Errorf("transport saw %d requests, want 1", stub.reqs)
	}
	calls := d.Calls()
	if len(calls) != 1 || calls[0] != (tagtest.Call{Service: "SQS", Operation: "CreateQueue", Tagged: true}) {
		t.Errorf("Calls = %+v", calls)
	}
}

func TestUntaggedCreateSecurityGroupFails(t *testing.T) {
	cfg, rec, d, _ := setup(t)
	_, _ = ec2.NewFromConfig(cfg).CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName: aws.String("sg"), Description: aws.String("d"),
	})
	wantFail(t, rec, "EC2 CreateSecurityGroup", tags.ManagedBy+"=spawn", `Operation: "CreateSecurityGroup"`)
	if c := d.Calls(); len(c) != 1 || c[0].Tagged || c[0].Allowed || c[0].Reason == "" {
		t.Errorf("Calls = %+v", c)
	}
}

func TestEC2TagSpecificationsRecognised(t *testing.T) {
	cfg, rec, _, _ := setup(t)
	_, _ = ec2.NewFromConfig(cfg).CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName: aws.String("sg"), Description: aws.String("d"),
		TagSpecifications: ec2tags.Specifications(spawnTags, ec2types.ResourceTypeSecurityGroup),
	})
	wantPass(t, rec)
}

func TestEC2EveryTagSpecificationNeedsManagedBy(t *testing.T) {
	cfg, rec, _, _ := setup(t)
	specs := ec2tags.Specifications(spawnTags, ec2types.ResourceTypeInstance)
	specs = append(specs, ec2types.TagSpecification{
		ResourceType: ec2types.ResourceTypeVolume,
		Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("data")}},
	})
	_, _ = ec2.NewFromConfig(cfg).RunInstances(ctx, &ec2.RunInstancesInput{
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1), TagSpecifications: specs,
	})
	wantFail(t, rec, "RunInstances", "TagSpecifications[1] (volume)")
}

func TestWrongToolFails(t *testing.T) {
	cfg, rec, _, _ := setup(t)
	other := tags.For(tags.ToolLagotto, "w", time.Unix(1, 0), "")
	_, _ = sns.NewFromConfig(cfg).CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("t"), Tags: snstags.FromMap(other)})
	wantFail(t, rec, "SNS CreateTopic", "=lagotto, want spawn")
}

func TestDefaultAllowListedOpPasses(t *testing.T) {
	cfg, rec, d, _ := setup(t)
	_, _ = iam.NewFromConfig(cfg).PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: aws.String("r"), PolicyName: aws.String("p"), PolicyDocument: aws.String("{}"),
	})
	_, _ = scheduler.NewFromConfig(cfg).CreateSchedule(ctx, &scheduler.CreateScheduleInput{Name: aws.String("s")})
	wantPass(t, rec)
	calls := d.Calls()
	if len(calls) != 2 || !calls[0].Allowed || !calls[1].Allowed || calls[0].Reason == "" {
		t.Errorf("Calls = %+v, want both allowed with a reason", calls)
	}
}

func TestCallerAllowListedOpPasses(t *testing.T) {
	cfg, rec, _, _ := setup(t, tagtest.Allow{
		Service: "IAM", Operation: "CreateInstanceProfile",
		Reason: "this flow tags the profile with TagInstanceProfile right after",
	})
	_, _ = iam.NewFromConfig(cfg).CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String("p")})
	wantPass(t, rec)
}

func TestAllowEntryWithoutReasonIsFatal(t *testing.T) {
	rec := &recorder{TB: t}
	tagtest.New(rec, tags.ToolSpawn, tagtest.Allow{Service: "IAM", Operation: "CreateRole"})
	if len(rec.fatals) != 1 || !strings.Contains(rec.fatals[0], "Reason") {
		t.Errorf("fatals = %q, want one about the missing Reason", rec.fatals)
	}
}

func TestNonCreateOpIgnored(t *testing.T) {
	cfg, rec, d, stub := setup(t)
	_, _ = ec2.NewFromConfig(cfg).DescribeInstances(ctx, &ec2.DescribeInstancesInput{})
	_, _ = sqs.NewFromConfig(cfg).SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String("u"), MessageBody: aws.String("b")})
	_, _ = ec2.NewFromConfig(cfg).TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{"i-1"}})
	wantPass(t, rec)
	if c := d.Calls(); len(c) != 0 {
		t.Errorf("Calls = %+v, want none", c)
	}
	if stub.reqs != 3 {
		t.Errorf("transport saw %d requests, want 3", stub.reqs)
	}
}

// The check runs before the SDK's input validation, so an untagged call is
// caught even when the test's input is incomplete.
func TestCheckedBeforeInputValidation(t *testing.T) {
	cfg, rec, _, stub := setup(t)
	_, err := sqs.NewFromConfig(cfg).CreateQueue(ctx, &sqs.CreateQueueInput{}) // no QueueName
	if err == nil || errors.Is(err, errNoNetwork) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	wantFail(t, rec, "SQS CreateQueue")
	if stub.reqs != 0 {
		t.Errorf("an invalid input reached the transport")
	}
}

func TestIAMTagSlice(t *testing.T) {
	cfg, rec, _, _ := setup(t)
	_, _ = iam.NewFromConfig(cfg).CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("r"), AssumeRolePolicyDocument: aws.String("{}"), Tags: iamtags.FromMap(spawnTags),
	})
	wantPass(t, rec)
}

func TestS3Shapes(t *testing.T) {
	t.Run("PutObject Tagging query string", func(t *testing.T) {
		cfg, rec, _, _ := setup(t)
		_, _ = s3.NewFromConfig(cfg).PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("b"), Key: aws.String("k"), Tagging: aws.String(s3tags.Query(spawnTags)),
		})
		wantPass(t, rec)
	})
	t.Run("PutObject without Tagging", func(t *testing.T) {
		cfg, rec, _, _ := setup(t)
		_, _ = s3.NewFromConfig(cfg).PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("b"), Key: aws.String("k")})
		wantFail(t, rec, "S3 PutObject", "no "+tags.ManagedBy+" tag in Tagging")
	})
	t.Run("PutBucketTagging TagSet", func(t *testing.T) {
		cfg, rec, _, _ := setup(t)
		_, _ = s3.NewFromConfig(cfg).PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
			Bucket: aws.String("b"), Tagging: s3tags.Tagging(spawnTags),
		})
		wantPass(t, rec)
	})
	t.Run("PutBucketTagging dropping managed-by", func(t *testing.T) {
		// PutBucketTagging replaces the whole set, so this would strip the tag.
		cfg, rec, _, _ := setup(t)
		_, _ = s3.NewFromConfig(cfg).PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
			Bucket: aws.String("b"), Tagging: s3tags.Tagging(map[string]string{"Name": "x"}),
		})
		wantFail(t, rec, "PutBucketTagging")
	})
	t.Run("CreateBucket configuration tags", func(t *testing.T) {
		cfg, rec, _, _ := setup(t)
		_, _ = s3.NewFromConfig(cfg).CreateBucket(ctx, &s3.CreateBucketInput{
			Bucket:                    aws.String("b"),
			CreateBucketConfiguration: &s3types.CreateBucketConfiguration{Tags: s3tags.FromMap(spawnTags)},
		})
		wantPass(t, rec)
	})
	t.Run("CreateBucket without configuration", func(t *testing.T) {
		cfg, rec, _, _ := setup(t)
		_, _ = s3.NewFromConfig(cfg).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("b")})
		wantFail(t, rec, "S3 CreateBucket", "CreateBucketConfiguration.Tags")
	})
}

func TestInputWithoutTagField(t *testing.T) {
	cfg, rec, _, _ := setup(t)
	// CreateBackup is a create op whose input has no tag field and is not allow-listed.
	_, _ = dynamodb.NewFromConfig(cfg).CreateBackup(ctx, &dynamodb.CreateBackupInput{
		BackupName: aws.String("b"), TableName: aws.String("t"),
	})
	wantFail(t, rec, "DynamoDB CreateBackup", "has no tag field")
}

func TestAPIOptionPerClient(t *testing.T) {
	rec := &recorder{TB: t}
	d := tagtest.New(rec, tags.ToolSpawn)
	cfg := aws.Config{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: &stubHTTP{},
		Retryer: func() aws.Retryer { return aws.NopRetryer{} }}
	client := ec2.NewFromConfig(cfg, func(o *ec2.Options) { o.APIOptions = append(o.APIOptions, d.APIOption) })
	_, _ = client.CreateKeyPair(ctx, &ec2.CreateKeyPairInput{KeyName: aws.String("k")})
	wantFail(t, rec, "EC2 CreateKeyPair")
}

func TestConcurrentCalls(t *testing.T) {
	cfg, rec, d, _ := setup(t)
	client := sqs.NewFromConfig(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("q"), Tags: spawnTags})
		}()
	}
	wg.Wait()
	wantPass(t, rec)
	if n := len(d.Calls()); n != 20 {
		t.Errorf("recorded %d calls, want 20", n)
	}
}

func TestIsCreate(t *testing.T) {
	for op, want := range map[string]bool{
		"CreateQueue": true, "PutObject": true, "RunInstances": true, "RegisterImage": true,
		"ImportKeyPair": true, "AllocateAddress": true, "CopyImage": true, "CopySnapshot": true,
		"RequestSpotInstances": true, "RequestSpotFleet": true,
		"DescribeInstances": false, "DeleteQueue": false, "TagResource": false,
		"SendMessage": false, "CopyObject": false, "UpdateItem": false,
	} {
		if got := tagtest.IsCreate(op); got != want {
			t.Errorf("IsCreate(%q) = %v, want %v", op, got, want)
		}
	}
}

func TestDefaultAllowListIsACopy(t *testing.T) {
	a := tagtest.DefaultAllowList()
	a[0].Reason = ""
	if tagtest.DefaultAllowList()[0].Reason == "" {
		t.Error("DefaultAllowList shares storage with the Detector's list")
	}
}
