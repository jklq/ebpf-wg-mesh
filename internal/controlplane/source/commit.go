package source

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// CommitContributor is one person credited on a commit: its author and every
// Co-authored-by trailer. Login and AvatarURL are empty when the person cannot
// be tied to a GitHub account.
type CommitContributor struct {
	Name      string `json:"name,omitempty"`
	Login     string `json:"login,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// CommitContributors is stored as a JSONB array, author first.
type CommitContributors []CommitContributor

func (c CommitContributors) Value() (driver.Value, error) {
	if len(c) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal([]CommitContributor(c))
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

func (c *CommitContributors) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case nil:
		*c = nil
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("scan commit contributors: unsupported type %T", src)
	}
	var out []CommitContributor
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("scan commit contributors: %w", err)
	}
	*c = out
	return nil
}

// CommitPerson is a raw identity from a GitHub commit payload, before
// contributors are resolved and deduplicated.
type CommitPerson struct {
	Name      string
	Email     string
	Login     string
	AvatarURL string
}

var (
	coAuthorTrailer = regexp.MustCompile(`(?mi)^co-authored-by:\s*(.+?)\s*<([^>]+)>\s*$`)
	noreplyEmail    = regexp.MustCompile(`(?i)^(?:\d+\+)?([a-z0-9](?:[a-z0-9-]*[a-z0-9])?)@users\.noreply\.github\.com$`)
)

// ResolveCommitContributors lists the commit's author followed by its
// Co-authored-by trailers. A person without a known login is matched to a
// GitHub account through a users.noreply.github.com address when possible.
func ResolveCommitContributors(author CommitPerson, message string) CommitContributors {
	people := []CommitPerson{author}
	for _, match := range coAuthorTrailer.FindAllStringSubmatch(message, -1) {
		people = append(people, CommitPerson{Name: match[1], Email: match[2]})
	}
	var out CommitContributors
	seen := make(map[string]struct{}, len(people))
	for _, person := range people {
		person.Name = strings.TrimSpace(person.Name)
		person.Email = strings.TrimSpace(person.Email)
		person.Login = strings.TrimSpace(person.Login)
		if person.Login == "" {
			if match := noreplyEmail.FindStringSubmatch(person.Email); match != nil {
				person.Login = match[1]
			}
		}
		key := strings.ToLower(person.Login)
		if key == "" {
			key = strings.ToLower(person.Email)
		}
		if key == "" {
			key = strings.ToLower(person.Name)
		}
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		avatar := strings.TrimSpace(person.AvatarURL)
		if avatar == "" && person.Login != "" {
			avatar = "https://github.com/" + person.Login + ".png"
		}
		name := person.Name
		if name == "" {
			name = person.Login
		}
		out = append(out, CommitContributor{Name: name, Login: person.Login, AvatarURL: avatar})
	}
	return out
}
