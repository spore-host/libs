package snstags

import (
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sns/types"

	"github.com/spore-host/libs/tags"
)

var sample = tags.For(tags.ToolSpawn, "run-1", time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), "bob")

func TestRoundTrip(t *testing.T) {
	ts := FromMap(sample)
	if len(ts) != len(sample) {
		t.Fatalf("FromMap gave %d tags, want %d", len(ts), len(sample))
	}
	for i := 1; i < len(ts); i++ {
		if *ts[i-1].Key >= *ts[i].Key {
			t.Errorf("FromMap not sorted: %q before %q", *ts[i-1].Key, *ts[i].Key)
		}
	}
	if got := ToMap(ts); !reflect.DeepEqual(got, sample) {
		t.Errorf("ToMap(FromMap(m)) = %v, want %v", got, sample)
	}
	if !tags.IsManagedBy(ToMap(ts), tags.ToolSpawn) {
		t.Error("round-tripped tags lost managed-by")
	}
}

func TestNilSafe(t *testing.T) {
	if FromMap(nil) != nil {
		t.Error("FromMap(nil) != nil")
	}
	k := "k"
	got := ToMap([]types.Tag{{Key: nil}, {Key: &k}})
	if !reflect.DeepEqual(got, map[string]string{"k": ""}) {
		t.Errorf("ToMap with a nil key and a nil value = %v", got)
	}
}
