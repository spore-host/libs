package lifecycle_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/tagtest"
)

// stubDynamo answers DynamoDB's JSON protocol from canned bodies keyed by the
// X-Amz-Target operation, so a real *dynamodb.Client (and its error
// deserialisers) can be driven without AWS.
type stubDynamo struct {
	mu        sync.Mutex
	responses map[string][]stubResponse
	calls     []string
}

type stubResponse struct {
	status int
	body   string
}

func (s *stubDynamo) RoundTrip(r *http.Request) (*http.Response, error) {
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "DynamoDB_20120810.")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, op)
	q := s.responses[op]
	if len(q) == 0 {
		return nil, errors.New("stub: unexpected " + op)
	}
	resp := q[0]
	if len(q) > 1 {
		s.responses[op] = q[1:]
	}
	return &http.Response{
		StatusCode: resp.status,
		Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}},
		Body:       io.NopCloser(strings.NewReader(resp.body)),
		Request:    r,
	}, nil
}

func awsError(typ, msg string) stubResponse {
	return stubResponse{400, `{"__type":"com.amazonaws.dynamodb.v20120810#` + typ + `","message":"` + msg + `"}`}
}

// TestRealSDKClient runs MarkActive through the real SDK client: the table is
// missing, gets created (tagged, as tagtest checks), and the activation
// transaction is cancelled by a live lease until the wait gives up. It proves
// the error matching works on the SDK's own deserialised errors, including
// the cancellation reasons and the old item they carry.
func TestRealSDKClient(t *testing.T) {
	lease := `{"M":{"token":{"S":"t"},"owner":{"S":"backstop-us-west-2"},"heartbeat":{"S":"2026-11-16T12:00:00.000Z"},"step":{"S":""}}}`
	stub := &stubDynamo{responses: map[string][]stubResponse{
		"DescribeTable": {awsError("ResourceNotFoundException", "Requested resource not found")},
		"CreateTable":   {{200, `{"TableDescription":{"TableName":"spore-host-lifecycle","TableStatus":"ACTIVE"}}`}},
		"TransactWriteItems": {
			awsError("ResourceNotFoundException", "Requested resource not found"),
			{400, `{"__type":"com.amazonaws.dynamodb.v20120810#TransactionCanceledException","message":"Transaction cancelled",` +
				`"CancellationReasons":[{"Code":"None"},{"Code":"ConditionalCheckFailed","Item":{"pk":{"S":"tool#lagotto#us-west-2"},"lease":` + lease + `}}]}`},
		},
	}}
	cfg := aws.Config{
		Region:           region,
		HTTPClient:       &http.Client{Transport: stub},
		RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}, nil
		}),
	}
	det := tagtest.New(t, tags.ToolShared)
	det.Install(&cfg)
	ddb := dynamodb.NewFromConfig(cfg)

	now := t0.Add(time.Minute)
	c, err := lifecycle.New(lifecycle.Options{
		DynamoDB:    func(string) (lifecycle.API, error) { return ddb, nil },
		Now:         func() time.Time { return now },
		Sleep:       func(context.Context, time.Duration) error { return nil },
		WaitTimeout: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = c.MarkActive(context.Background(), tool, region)
	var we *lifecycle.WaitError
	if !errors.As(err, &we) {
		t.Fatalf("MarkActive = %v, want a WaitError (stub calls %v)", err, stub.calls)
	}
	if we.Blocker != "lease" || we.Owner != "backstop-us-west-2" || !we.Heartbeat.Equal(t0) {
		t.Errorf("WaitError = %+v", we)
	}
	want := []string{"TransactWriteItems", "DescribeTable", "CreateTable", "TransactWriteItems", "TransactWriteItems"}
	if strings.Join(stub.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", stub.calls, want)
	}
	created := false
	for _, call := range det.Calls() {
		if call.Operation == "CreateTable" {
			created = call.Tagged
		}
	}
	if !created {
		t.Errorf("CreateTable was not seen tagged managed-by=%s: %+v", tags.ToolShared, det.Calls())
	}
}
