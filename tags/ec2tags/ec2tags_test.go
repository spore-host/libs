package ec2tags

import (
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

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

func TestSpecifications(t *testing.T) {
	specs := Specifications(sample, types.ResourceTypeInstance, types.ResourceTypeVolume)
	if len(specs) != 2 || specs[0].ResourceType != types.ResourceTypeInstance || specs[1].ResourceType != types.ResourceTypeVolume {
		t.Fatalf("Specifications = %+v", specs)
	}
	for _, s := range specs {
		if !tags.IsManagedBy(ToMap(s.Tags), tags.ToolSpawn) {
			t.Errorf("spec %s lacks managed-by", s.ResourceType)
		}
	}
	// Each spec has its own tags, so editing one cannot change another.
	*specs[0].Tags[0].Value = "changed"
	if *specs[1].Tags[0].Value == "changed" {
		t.Error("specs share tag storage")
	}
	if Specifications(sample) != nil || Specifications(nil, types.ResourceTypeInstance) != nil {
		t.Error("Specifications with no types or no tags != nil")
	}
}
