package pairs

import (
	"reflect"
	"testing"
)

type kv struct{ k, v *string }

func mk(k, v *string) kv         { return kv{k, v} }
func rd(e kv) (*string, *string) { return e.k, e.v }

func TestSliceSortedAndDistinct(t *testing.T) {
	got := Slice(map[string]string{"b": "2", "a": "1", "c": "3"}, mk)
	if len(got) != 3 || *got[0].k != "a" || *got[1].k != "b" || *got[2].k != "c" || *got[0].v != "1" {
		t.Fatalf("Slice = %+v", got)
	}
	if got[0].k == got[1].k {
		t.Error("elements share a key pointer")
	}
	if Slice[kv](nil, mk) != nil || Slice(map[string]string{}, mk) != nil {
		t.Error("empty input did not yield nil")
	}
}

func TestMap(t *testing.T) {
	a, one, b := "a", "1", "b"
	got := Map([]kv{{&a, &one}, {nil, &one}, {&b, nil}}, rd)
	if !reflect.DeepEqual(got, map[string]string{"a": "1", "b": ""}) {
		t.Errorf("Map = %v", got)
	}
	if m := Map[kv](nil, rd); m == nil || len(m) != 0 {
		t.Errorf("Map(nil) = %#v, want empty non-nil", m)
	}
}
