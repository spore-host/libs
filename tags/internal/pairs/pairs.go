// Package pairs converts between a tag map and the {Key, Value} struct slices
// that most AWS SDK v2 services use, for the per-service converter packages.
package pairs

import "sort"

// Slice returns one element per entry of m, built by mk, in key order so that
// the result (and any request built from it) is deterministic. A nil or empty
// map yields nil.
func Slice[T any](m map[string]string, mk func(k, v *string) T) []T {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]T, 0, len(keys))
	for _, k := range keys {
		k, v := k, m[k]
		out = append(out, mk(&k, &v))
	}
	return out
}

// Map returns the tags in ts as a map, using kv to read each element's key and
// value. An element with a nil key is skipped; a nil value reads as "". A later
// duplicate key wins. An empty slice yields an empty, non-nil map.
func Map[T any](ts []T, kv func(T) (k, v *string)) map[string]string {
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		k, v := kv(t)
		if k == nil {
			continue
		}
		if v == nil {
			m[*k] = ""
			continue
		}
		m[*k] = *v
	}
	return m
}
