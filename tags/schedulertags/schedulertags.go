// Package schedulertags converts spore-host tag maps (see package tags) to and from
// the EventBridge Scheduler tag type, github.com/aws/aws-sdk-go-v2/service/scheduler/types.Tag.
//
// Use it for CreateScheduleGroup (Tags). A schedule group can be tagged and a
// schedule cannot, so a tool tags the group and puts its schedules in it.
//
// It imports only that types package, not the EventBridge Scheduler client.
package schedulertags

import (
	"github.com/aws/aws-sdk-go-v2/service/scheduler/types"

	"github.com/spore-host/libs/tags/internal/pairs"
)

// FromMap returns m as EventBridge Scheduler tags, sorted by key. A nil or empty map yields nil.
func FromMap(m map[string]string) []types.Tag {
	return pairs.Slice(m, func(k, v *string) types.Tag { return types.Tag{Key: k, Value: v} })
}

// ToMap returns ts as a map, for use with tags.IsManagedBy and friends. A tag
// with a nil key is skipped and a nil value reads as "".
func ToMap(ts []types.Tag) map[string]string {
	return pairs.Map(ts, func(t types.Tag) (*string, *string) { return t.Key, t.Value })
}
