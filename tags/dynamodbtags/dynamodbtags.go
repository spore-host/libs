// Package dynamodbtags converts spore-host tag maps (see package tags) to and from
// the DynamoDB tag type, github.com/aws/aws-sdk-go-v2/service/dynamodb/types.Tag.
//
// Use it for CreateTable (Tags).
//
// It imports only that types package, not the DynamoDB client.
package dynamodbtags

import (
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/tags/internal/pairs"
)

// FromMap returns m as DynamoDB tags, sorted by key. A nil or empty map yields nil.
func FromMap(m map[string]string) []types.Tag {
	return pairs.Slice(m, func(k, v *string) types.Tag { return types.Tag{Key: k, Value: v} })
}

// ToMap returns ts as a map, for use with tags.IsManagedBy and friends. A tag
// with a nil key is skipped and a nil value reads as "".
func ToMap(ts []types.Tag) map[string]string {
	return pairs.Map(ts, func(t types.Tag) (*string, *string) { return t.Key, t.Value })
}
