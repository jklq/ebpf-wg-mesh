package delivery

import "strings"

func volumeKey(environmentID, name string) string {
	var b strings.Builder
	b.Grow(len(environmentID) + 1 + len(name))
	b.WriteString(environmentID)
	b.WriteByte(0)
	b.WriteString(name)
	return b.String()
}
