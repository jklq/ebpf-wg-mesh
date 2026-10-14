package journal

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"testing"
)

func TestRowMapPreservesPrefixesAndRoundTrips(t *testing.T) {
	var rows RowMap[int]
	want := make(map[string]int)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 1000 {
		old, previous := rows, maps.Clone(want)
		key := fmt.Sprint(rng.IntN(100))
		if rng.IntN(3) == 0 {
			rows.Delete(key)
			delete(want, key)
		} else {
			rows.Set(key, i)
			want[key] = i
		}
		if !maps.Equal(maps.Collect(old.All()), previous) {
			t.Fatal("editing changed a previous prefix")
		}
		if rows.Len() != len(want) || !maps.Equal(maps.Collect(rows.All()), want) {
			t.Fatal("map differs from expected rows")
		}
		for key, value := range want {
			if got, ok := rows.Lookup(key); !ok || got != value {
				t.Fatalf("lookup %s = %d, %t", key, got, ok)
			}
		}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var imported RowMap[int]
	if err := json.Unmarshal(raw, &imported); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, imported) {
		t.Fatal("import must rebuild the same canonical tree")
	}
}

func BenchmarkAssignmentUpdate(b *testing.B) {
	for _, size := range []int{1000, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			var rows RowMap[Assignment]
			for i := range size {
				key := fmt.Sprint(i)
				rows.Set(key, Assignment{ID: key})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				next := rows
				next.Set("500", Assignment{ID: "500", OperatorRestartNonce: int64(i)})
			}
		})
	}
}
