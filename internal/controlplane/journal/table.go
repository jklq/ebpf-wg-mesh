package journal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"iter"
	"maps"
)

// RowMap shares immutable tree nodes between journal prefixes. Updating a key
// copies its search path, rather than every row. Set and Delete change only the
// receiving table; copy the table value before editing a borrowed projection.
type RowMap[T any] struct{ root *tableNode[T] }

type tableNode[T any] struct {
	key         string
	value       T
	priority    uint64
	size        int
	left, right *tableNode[T]
}

func NewRowMap[T any](rows map[string]T) RowMap[T] {
	var table RowMap[T]
	for key, value := range rows {
		table.Set(key, value)
	}
	return table
}

func (t RowMap[T]) Len() int { return nodeSize(t.root) }

func (t RowMap[T]) Lookup(key string) (T, bool) {
	for n := t.root; n != nil; {
		switch {
		case key < n.key:
			n = n.left
		case key > n.key:
			n = n.right
		default:
			return n.value, true
		}
	}
	var zero T
	return zero, false
}

func (t RowMap[T]) Get(key string) T { value, _ := t.Lookup(key); return value }

func (t *RowMap[T]) Set(key string, value T) {
	sum := sha256.Sum256([]byte(key))
	t.root = setNode(t.root, &tableNode[T]{key: key, value: value, priority: binary.BigEndian.Uint64(sum[:8]), size: 1})
}

func (t *RowMap[T]) Delete(key string) { t.root = deleteNode(t.root, key) }

func (t RowMap[T]) All() iter.Seq2[string, T] {
	return func(yield func(string, T) bool) { walkRowMap(t.root, yield) }
}

func walkRowMap[T any](n *tableNode[T], yield func(string, T) bool) bool {
	return n == nil || walkRowMap(n.left, yield) && yield(n.key, n.value) && walkRowMap(n.right, yield)
}

func (t RowMap[T]) MarshalJSON() ([]byte, error) { return json.Marshal(maps.Collect(t.All())) }

func (t *RowMap[T]) UnmarshalJSON(raw []byte) error {
	var rows map[string]T
	if err := json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	*t = NewRowMap(rows)
	return nil
}

func nodeSize[T any](n *tableNode[T]) int {
	if n == nil {
		return 0
	}
	return n.size
}

func sizedNode[T any](n tableNode[T]) *tableNode[T] {
	n.size = 1 + nodeSize(n.left) + nodeSize(n.right)
	return &n
}

func precedes[T any](a, b *tableNode[T]) bool {
	return a.priority < b.priority || a.priority == b.priority && a.key < b.key
}

func setNode[T any](previous, value *tableNode[T]) *tableNode[T] {
	if previous == nil {
		return value
	}
	n := *previous
	if value.key == n.key {
		n.value = value.value
	} else if value.key < n.key {
		n.left = setNode(n.left, value)
		if precedes(n.left, &n) {
			root := *n.left
			n.left = root.right
			root.right = sizedNode(n)
			return sizedNode(root)
		}
	} else {
		n.right = setNode(n.right, value)
		if precedes(n.right, &n) {
			root := *n.right
			n.right = root.left
			root.left = sizedNode(n)
			return sizedNode(root)
		}
	}
	return sizedNode(n)
}

func deleteNode[T any](previous *tableNode[T], key string) *tableNode[T] {
	if previous == nil {
		return nil
	}
	n := *previous
	if key < n.key {
		n.left = deleteNode(n.left, key)
	} else if key > n.key {
		n.right = deleteNode(n.right, key)
	} else {
		return joinNodes(n.left, n.right)
	}
	return sizedNode(n)
}

func joinNodes[T any](left, right *tableNode[T]) *tableNode[T] {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	if precedes(left, right) {
		n := *left
		n.right = joinNodes(n.right, right)
		return sizedNode(n)
	}
	n := *right
	n.left = joinNodes(left, n.left)
	return sizedNode(n)
}
