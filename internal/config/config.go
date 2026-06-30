package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultIncludePrivate  = true
	DefaultIncludeArchived = true
	DefaultMirrorInterval  = 8 * time.Hour
	DefaultStatePath       = "~/.config/github-gitea-sync/state.json"
	DefaultLogFormat       = "text"
)

type LookupEnv func(string) (string, bool)

type Config struct {
	GitHubToken     string
	GiteaToken      string
	GiteaBaseURL    string
	GitHubAccounts  []string
	IncludePrivate  bool
	IncludeArchived bool
	MirrorInterval  time.Duration
	StatePath       string
	LogFormat       string
}

type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	if len(e.Problems) == 0 {
		return "ERROR_CONFIG: invalid configuration"
	}
	return "ERROR_CONFIG: " + strings.Join(e.Problems, "; ")
}

func Load(lookup LookupEnv) (Config, error) {
	cfg := Config{
		IncludePrivate:  DefaultIncludePrivate,
		IncludeArchived: DefaultIncludeArchived,
		MirrorInterval:  DefaultMirrorInterval,
		StatePath:       DefaultStatePath,
		LogFormat:       DefaultLogFormat,
	}
	var problems []string

	cfg.GitHubToken = required(lookup, "GGS_GITHUB_TOKEN", &problems)
	cfg.GiteaToken = required(lookup, "GGS_GITEA_TOKEN", &problems)
	cfg.GiteaBaseURL = required(lookup, "GGS_GITEA_BASE_URL", &problems)
	accountsRaw := required(lookup, "GGS_GITHUB_ACCOUNTS", &problems)

	if cfg.GiteaBaseURL != "" {
		parsed, err := url.ParseRequestURI(cfg.GiteaBaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			problems = append(problems, "GGS_GITEA_BASE_URL must be an absolute URL")
		}
	}

	if accountsRaw != "" {
		accounts, err := parseAccounts(accountsRaw)
		if err != nil {
			problems = append(problems, err.Error())
		} else {
			cfg.GitHubAccounts = accounts
		}
	}

	if value, ok := lookup("GGS_INCLUDE_PRIVATE"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			problems = append(problems, "GGS_INCLUDE_PRIVATE must be a boolean")
		} else {
			cfg.IncludePrivate = parsed
		}
	}

	if value, ok := lookup("GGS_INCLUDE_ARCHIVED"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			problems = append(problems, "GGS_INCLUDE_ARCHIVED must be a boolean")
		} else {
			cfg.IncludeArchived = parsed
		}
	}

	if value, ok := lookup("GGS_MIRROR_INTERVAL"); ok {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			problems = append(problems, "GGS_MIRROR_INTERVAL must be a positive duration")
		} else {
			cfg.MirrorInterval = parsed
		}
	}

	if value, ok := lookup("GGS_STATE_PATH"); ok {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, "GGS_STATE_PATH must not be empty")
		} else {
			cfg.StatePath = value
		}
	}

	if value, ok := lookup("GGS_LOG_FORMAT"); ok {
		if value != DefaultLogFormat {
			problems = append(problems, "GGS_LOG_FORMAT supports only text")
		} else {
			cfg.LogFormat = value
		}
	}

	if len(problems) > 0 {
		return Config{}, &Error{Problems: problems}
	}
	return cfg, nil
}

func required(lookup LookupEnv, name string, problems *[]string) string {
	value, ok := lookup(name)
	if !ok || strings.TrimSpace(value) == "" {
		*problems = append(*problems, fmt.Sprintf("%s is required", name))
		return ""
	}
	return value
}

func parseAccounts(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	accounts := make([]string, 0, len(parts))
	for _, part := range parts {
		account := strings.TrimSpace(part)
		if account == "" {
			return nil, errors.New("GGS_GITHUB_ACCOUNTS must not contain empty entries")
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}
