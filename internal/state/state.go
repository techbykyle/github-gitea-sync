package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github-gitea-sync/internal/redact"
)

const CurrentVersion = 1

type State struct {
	Version int         `json:"version"`
	GitHub  GitHubState `json:"github"`
}

type GitHubState struct {
	LastSuccessfulInventoryAt *time.Time                      `json:"last_successful_inventory_at,omitempty"`
	Owners                    map[string]GitHubOwnerState     `json:"owners,omitempty"`
	RepositoriesByID          map[string]GitHubRepository     `json:"repositories_by_id,omitempty"`
	ConditionalMetadata       map[string]ConditionalMetadata  `json:"conditional_metadata,omitempty"`
	RepositoryPages           map[string]GitHubRepositoryPage `json:"repository_pages,omitempty"`
}

type GitHubOwnerState struct {
	Configured                string             `json:"configured"`
	Login                     string             `json:"login"`
	Type                      string             `json:"type"`
	LastSuccessfulInventoryAt time.Time          `json:"last_successful_inventory_at"`
	Repositories              []GitHubRepository `json:"repositories,omitempty"`
	SkippedForks              []GitHubRepository `json:"skipped_forks,omitempty"`
}

type GitHubRepository struct {
	ID       int64  `json:"id"`
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	Fork     bool   `json:"fork"`
	HTMLURL  string `json:"html_url,omitempty"`
	CloneURL string `json:"clone_url,omitempty"`
	SSHURL   string `json:"ssh_url,omitempty"`
}

type GitHubRepositoryPage struct {
	Repositories []GitHubRepository `json:"repositories,omitempty"`
	Full         bool               `json:"full"`
}

type ConditionalMetadata struct {
	ETag           string    `json:"etag"`
	LastObservedAt time.Time `json:"last_observed_at"`
}

func New() State {
	return State{
		Version: CurrentVersion,
		GitHub: GitHubState{
			Owners:              make(map[string]GitHubOwnerState),
			RepositoriesByID:    make(map[string]GitHubRepository),
			ConditionalMetadata: make(map[string]ConditionalMetadata),
			RepositoryPages:     make(map[string]GitHubRepositoryPage),
		},
	}
}

func Load(path string) (State, string, error) {
	expanded, err := ExpandPath(path)
	if err != nil {
		return State{}, "", err
	}
	data, err := os.ReadFile(expanded)
	if errors.Is(err, os.ErrNotExist) {
		return New(), expanded, nil
	}
	if err != nil {
		return State{}, expanded, fmt.Errorf("ERROR_STATE: read %s: %w", expanded, err)
	}

	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		return State{}, expanded, fmt.Errorf("ERROR_STATE: corrupt JSON in %s: %w", expanded, err)
	}
	if loaded.Version != CurrentVersion {
		return State{}, expanded, fmt.Errorf("ERROR_STATE: unsupported state version %d in %s", loaded.Version, expanded)
	}
	loaded.ensureMaps()
	return loaded, expanded, nil
}

func Save(path string, st State, prohibitedSecrets ...string) error {
	expanded, err := ExpandPath(path)
	if err != nil {
		return err
	}
	st.Version = CurrentVersion
	st.ensureMaps()
	if err := validate(st, prohibitedSecrets...); err != nil {
		return err
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("ERROR_STATE: encode state: %w", err)
	}
	data = append(data, '\n')
	if redact.ContainsSecret(string(data), prohibitedSecrets...) {
		return fmt.Errorf("ERROR_STATE: refusing to write state containing a configured secret")
	}

	dir := filepath.Dir(expanded)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ERROR_STATE: create state directory %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("ERROR_STATE: create temporary state file in %s: %w", dir, err)
	}
	tempName := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempName)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("ERROR_STATE: secure temporary state file %s: %w", tempName, err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("ERROR_STATE: write temporary state file %s: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("ERROR_STATE: close temporary state file %s: %w", tempName, err)
	}
	if err := os.Rename(tempName, expanded); err != nil {
		return fmt.Errorf("ERROR_STATE: replace state file %s: %w", expanded, err)
	}
	removeTemp = false
	return nil
}

func ExpandPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("ERROR_STATE: state path must not be empty")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("ERROR_STATE: resolve home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("ERROR_STATE: unsupported state path %q", path)
	}
	return path, nil
}

func (s *State) ensureMaps() {
	if s.Version == 0 {
		s.Version = CurrentVersion
	}
	if s.GitHub.Owners == nil {
		s.GitHub.Owners = make(map[string]GitHubOwnerState)
	}
	if s.GitHub.RepositoriesByID == nil {
		s.GitHub.RepositoriesByID = make(map[string]GitHubRepository)
	}
	if s.GitHub.ConditionalMetadata == nil {
		s.GitHub.ConditionalMetadata = make(map[string]ConditionalMetadata)
	}
	if s.GitHub.RepositoryPages == nil {
		s.GitHub.RepositoryPages = make(map[string]GitHubRepositoryPage)
	}
}

func validate(st State, prohibitedSecrets ...string) error {
	for _, owner := range st.GitHub.Owners {
		for _, repo := range append(owner.Repositories, owner.SkippedForks...) {
			if err := validateRepository(repo, prohibitedSecrets...); err != nil {
				return err
			}
		}
	}
	for _, repo := range st.GitHub.RepositoriesByID {
		if err := validateRepository(repo, prohibitedSecrets...); err != nil {
			return err
		}
	}
	for _, page := range st.GitHub.RepositoryPages {
		for _, repo := range page.Repositories {
			if err := validateRepository(repo, prohibitedSecrets...); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRepository(repo GitHubRepository, prohibitedSecrets ...string) error {
	values := []string{repo.HTMLURL, repo.CloneURL, repo.SSHURL}
	for _, value := range values {
		if value == "" {
			continue
		}
		if redact.URLHasCredentials(value) {
			return fmt.Errorf("ERROR_STATE: refusing to write credential-bearing URL for %s", repo.FullName)
		}
		if redact.ContainsSecret(value, prohibitedSecrets...) {
			return fmt.Errorf("ERROR_STATE: refusing to write secret-bearing URL for %s", repo.FullName)
		}
	}
	return nil
}
