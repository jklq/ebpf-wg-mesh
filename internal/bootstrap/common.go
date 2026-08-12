package bootstrap

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"ebof-wg-mesh/internal/config"
)

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func envOrInt64(key string, fallback int64) int64 {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func envOrBool(key string, fallback bool) bool {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return fallback
}

type bootstrapUsersFlag struct {
	users *[]bootstrapUserSpec
}

type bootstrapUserSpec struct {
	userID   string
	email    string
	projects []string
}

func (f bootstrapUsersFlag) String() string {
	if f.users == nil || len(*f.users) == 0 {
		return ""
	}
	parts := make([]string, 0, len(*f.users))
	for _, user := range *f.users {
		parts = append(parts, fmt.Sprintf("%s:%s:%s", user.userID, user.email, strings.Join(user.projects, ",")))
	}
	return strings.Join(parts, ";")
}

func (f bootstrapUsersFlag) Set(value string) error {
	parts := strings.SplitN(value, ":", 3)
	if len(parts) < 2 {
		return fmt.Errorf("bootstrap user must be user-id:email[:project1,project2]")
	}
	spec := bootstrapUserSpec{
		userID: strings.TrimSpace(parts[0]),
		email:  strings.TrimSpace(parts[1]),
	}
	if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
		for _, item := range strings.Split(parts[2], ",") {
			project := strings.TrimSpace(item)
			if project != "" {
				spec.projects = append(spec.projects, project)
			}
		}
	}
	if spec.userID == "" || spec.email == "" {
		return fmt.Errorf("bootstrap user ID and email are required")
	}
	*f.users = append(*f.users, spec)
	return nil
}

func stringFlag(fs *flag.FlagSet, target *string, name, envKey, fallback, usage string) {
	fs.StringVar(target, name, envOr(envKey, fallback), usage)
}

func intFlag(fs *flag.FlagSet, target *int, name, envKey string, fallback int, usage string) {
	fs.IntVar(target, name, envOrInt(envKey, fallback), usage)
}

func int64Flag(fs *flag.FlagSet, target *int64, name, envKey string, fallback int64, usage string) {
	fs.Int64Var(target, name, envOrInt64(envKey, fallback), usage)
}

func boolFlag(fs *flag.FlagSet, target *bool, name, envKey string, fallback bool, usage string) {
	fs.BoolVar(target, name, envOrBool(envKey, fallback), usage)
}

func splitCommaList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	items := strings.Split(raw, ",")
	values := make([]string, 0, len(items))
	for _, item := range items {
		value := strings.TrimSpace(item)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func parseAgentBootstrapTokens(raw string) ([]config.AgentBootstrapToken, error) {
	values := splitCommaList(raw)
	tokens := make([]config.AgentBootstrapToken, 0, len(values))
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("agent bootstrap token must be agent_id=token")
		}
		tokens = append(tokens, config.AgentBootstrapToken{
			AgentID: strings.TrimSpace(parts[0]),
			Token:   strings.TrimSpace(parts[1]),
		})
	}
	return tokens, nil
}

func splitWhitespaceList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Fields(raw)
}

func parseEnvPairs(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	values := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		item := strings.TrimSpace(pair)
		if item == "" {
			continue
		}
		key, value, ok := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		values[key] = value
	}
	if len(values) == 0 {
		return nil
	}
	return values
}
