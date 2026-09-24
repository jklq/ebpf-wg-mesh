package logpipeline

// MaxBatchBytes caps the marshaled line payload of one shipped batch.
// A full batch of maximum-size lines stays well under gRPC's default
// 4 MiB receive limit even with the batch's drop summaries attached,
// so valid traffic can never wedge delivery behind a message too
// large to send.
const MaxBatchBytes = 2 << 20

// ChunkByBytes splits items into consecutive groups whose measured
// sizes sum to at most max per group, preserving order. An item
// larger than max forms its own group. An empty input yields no
// groups: callers that must always ship one message form an empty
// group themselves.
func ChunkByBytes[T any](items []T, size func(T) int, max int) [][]T {
	var chunks [][]T
	var cur []T
	curBytes := 0
	for _, item := range items {
		n := size(item)
		if len(cur) > 0 && curBytes+n > max {
			chunks = append(chunks, cur)
			cur = nil
			curBytes = 0
		}
		cur = append(cur, item)
		curBytes += n
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}
