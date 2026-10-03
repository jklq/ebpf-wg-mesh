package source

import (
	"reflect"
	"testing"
)

func TestResolveCommitContributors(t *testing.T) {
	t.Parallel()
	message := "Update index.html\n\n" +
		"Co-authored-by: Carol <42+carol@users.noreply.github.com>\n" +
		"co-authored-by: Dave <dave@example.com>\n" +
		"Co-authored-by: Ada Again <ADA@users.noreply.github.com>\n"
	got := ResolveCommitContributors(CommitPerson{
		Name:      "Ada",
		Email:     "ada@example.com",
		Login:     "ada",
		AvatarURL: "https://avatars.githubusercontent.com/u/1?v=4",
	}, message)
	want := CommitContributors{
		{Name: "Ada", Login: "ada", AvatarURL: "https://avatars.githubusercontent.com/u/1?v=4"},
		{Name: "Carol", Login: "carol", AvatarURL: "https://github.com/carol.png"},
		{Name: "Dave"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contributors = %+v, want %+v", got, want)
	}
}

func TestResolveCommitContributorsSkipsAnonymousAuthor(t *testing.T) {
	t.Parallel()
	if got := ResolveCommitContributors(CommitPerson{}, "No trailers"); len(got) != 0 {
		t.Fatalf("contributors = %+v, want none", got)
	}
}

func TestCommitContributorsSQLRoundTrip(t *testing.T) {
	t.Parallel()
	want := CommitContributors{{Name: "Ada", Login: "ada", AvatarURL: "https://github.com/ada.png"}}
	value, err := want.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	var got CommitContributors
	if err := got.Scan([]byte(value.(string))); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}
