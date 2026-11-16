// Package s3tags converts spore-host tag maps (see package tags) to and from
// the S3 tag type, github.com/aws/aws-sdk-go-v2/service/s3/types.Tag.
//
// Use Tagging for PutBucketTagging, FromMap for
// CreateBucketConfiguration.Tags on CreateBucket, and Query for the Tagging
// string of PutObject, CopyObject, and CreateMultipartUpload.
//
// It imports only that types package, not the S3 client.
package s3tags

import (
	"net/url"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/spore-host/libs/tags/internal/pairs"
)

// FromMap returns m as S3 tags, sorted by key. A nil or empty map yields nil.
func FromMap(m map[string]string) []types.Tag {
	return pairs.Slice(m, func(k, v *string) types.Tag { return types.Tag{Key: k, Value: v} })
}

// ToMap returns ts as a map, for use with tags.IsManagedBy and friends. A tag
// with a nil key is skipped and a nil value reads as "".
func ToMap(ts []types.Tag) map[string]string {
	return pairs.Map(ts, func(t types.Tag) (*string, *string) { return t.Key, t.Value })
}

// Tagging returns m as the *types.Tagging that PutBucketTagging takes.
// PutBucketTagging replaces the bucket's whole tag set, so pass the full set
// (including the spore-host tags), not only the tags being added.
func Tagging(m map[string]string) *types.Tagging {
	return &types.Tagging{TagSet: FromMap(m)}
}

// Query returns m URL-encoded as key1=value1&key2=value2, sorted by key: the
// form S3 takes in the Tagging field of PutObject, CopyObject, and
// CreateMultipartUpload. A nil or empty map yields "".
func Query(m map[string]string) string {
	v := make(url.Values, len(m))
	for k, val := range m {
		v.Set(k, val)
	}
	return v.Encode()
}
