// Package ec2tags converts spore-host tag maps (see package tags) to and from
// the EC2 tag type, github.com/aws/aws-sdk-go-v2/service/ec2/types.Tag.
//
// Most EC2 create calls take tags as TagSpecifications; use Specifications for
// those and FromMap for the few that take a plain Tags list (CreateTags).
//
// It imports only that types package, not the EC2 client.
package ec2tags

import (
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/libs/tags/internal/pairs"
)

// FromMap returns m as EC2 tags, sorted by key. A nil or empty map yields nil.
func FromMap(m map[string]string) []types.Tag {
	return pairs.Slice(m, func(k, v *string) types.Tag { return types.Tag{Key: k, Value: v} })
}

// ToMap returns ts as a map, for use with tags.IsManagedBy and friends. A tag
// with a nil key is skipped and a nil value reads as "".
func ToMap(ts []types.Tag) map[string]string {
	return pairs.Map(ts, func(t types.Tag) (*string, *string) { return t.Key, t.Value })
}

// Specifications returns one TagSpecification per resource type, each carrying
// all of m. Pass every resource type the call creates (for RunInstances that is
// usually ResourceTypeInstance, ResourceTypeVolume, and
// ResourceTypeNetworkInterface), so none of them is left untagged. A nil or empty
// map, or no resource types, yields nil.
func Specifications(m map[string]string, resourceTypes ...types.ResourceType) []types.TagSpecification {
	if len(m) == 0 || len(resourceTypes) == 0 {
		return nil
	}
	out := make([]types.TagSpecification, 0, len(resourceTypes))
	for _, rt := range resourceTypes {
		out = append(out, types.TagSpecification{ResourceType: rt, Tags: FromMap(m)})
	}
	return out
}
