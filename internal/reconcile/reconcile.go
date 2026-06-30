package reconcile

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github-gitea-sync/internal/gitea"
	"github-gitea-sync/internal/github"
	"github-gitea-sync/internal/redact"
)

type Command string

const (
	CommandPlan   Command = "plan"
	CommandVerify Command = "verify"
	CommandSync   Command = "sync"
)

type Status string

const (
	StatusOKMirrored                  Status = "OK_MIRRORED"
	StatusCreateMirror                Status = "CREATE_MIRROR"
	StatusCreatedMirror               Status = "CREATED_MIRROR"
	StatusSkippedFork                 Status = "SKIPPED_FORK"
	StatusBlockedMissingGiteaOwner    Status = "BLOCKED_MISSING_GITEA_OWNER"
	StatusBlockedOwnerTypeMismatch    Status = "BLOCKED_OWNER_TYPE_MISMATCH"
	StatusBlockedExistingNonMirror    Status = "BLOCKED_EXISTING_NON_MIRROR"
	StatusBlockedMirrorSourceMismatch Status = "BLOCKED_MIRROR_SOURCE_MISMATCH"
	StatusWarnMirrorSourceUnverified  Status = "WARN_MIRROR_SOURCE_UNVERIFIED"
	StatusWarnDuplicateName           Status = "WARN_DUPLICATE_NAME"
	StatusReportGitHubSourceMissing   Status = "REPORT_GITHUB_SOURCE_MISSING"
	StatusErrorConfig                 Status = "ERROR_CONFIG"
	StatusErrorGitHubAccess           Status = "ERROR_GITHUB_ACCESS"
	StatusErrorRateLimited            Status = "ERROR_RATE_LIMITED"
	StatusErrorGiteaAccess            Status = "ERROR_GITEA_ACCESS"
	StatusErrorInternal               Status = "ERROR_INTERNAL"
)

type Severity string

const (
	SeverityOK      Severity = "ok"
	SeverityAction  Severity = "action"
	SeveritySkipped Severity = "skipped"
	SeverityWarning Severity = "warning"
	SeverityBlocked Severity = "blocked"
	SeverityError   Severity = "error"
)

type Entry struct {
	Status           Status   `json:"status"`
	Severity         Severity `json:"severity"`
	Owner            string   `json:"owner,omitempty"`
	Repository       string   `json:"repository,omitempty"`
	GitHubOwner      string   `json:"github_owner,omitempty"`
	GitHubRepository string   `json:"github_repository,omitempty"`
	GitHubFullName   string   `json:"github_full_name,omitempty"`
	GiteaOwner       string   `json:"gitea_owner,omitempty"`
	GiteaRepository  string   `json:"gitea_repository,omitempty"`
	Private          bool     `json:"private,omitempty"`
	Archived         bool     `json:"archived,omitempty"`
	ExpectedSource   string   `json:"expected_source,omitempty"`
	ObservedSource   string   `json:"observed_source,omitempty"`
	Message          string   `json:"message,omitempty"`
	Error            string   `json:"error,omitempty"`
}

type Summary struct {
	Total    int            `json:"total"`
	OK       int            `json:"ok"`
	Actions  int            `json:"actions"`
	Created  int            `json:"created"`
	Skipped  int            `json:"skipped"`
	Warnings int            `json:"warnings"`
	Blocked  int            `json:"blocked"`
	Errors   int            `json:"errors"`
	ByStatus map[Status]int `json:"by_status"`
}

type Report struct {
	Command      Command   `json:"command"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	GiteaBaseURL string    `json:"gitea_base_url,omitempty"`
	Summary      Summary   `json:"summary"`
	Results      []Entry   `json:"results"`
}

type OwnerFailure struct {
	Owner   string
	Status  Status
	Message string
	Err     error
}

func Build(command Command, gh github.InventoryResult, giteaInventories map[string]gitea.OwnerInventory, ownerFailures map[string]OwnerFailure) []Entry {
	var entries []Entry
	duplicateRepos := duplicateGiteaRepositoryNames(giteaInventories)

	for _, ownerInventory := range gh.Owners {
		ownerKey := strings.ToLower(ownerInventory.Owner.Login)
		if failure, ok := ownerFailures[ownerKey]; ok {
			entries = append(entries, ownerFailureEntry(failure, ownerInventory.Owner))
			entries = append(entries, skippedForkEntries(ownerInventory)...)
			continue
		}

		giteaInventory, ok := giteaInventories[ownerKey]
		if !ok {
			entries = append(entries, Entry{
				Status:      StatusErrorGiteaAccess,
				Severity:    SeverityError,
				Owner:       ownerInventory.Owner.Login,
				GitHubOwner: ownerInventory.Owner.Login,
				GiteaOwner:  ownerInventory.Owner.Login,
				Message:     "Gitea owner inventory was not available",
			})
			entries = append(entries, skippedForkEntries(ownerInventory)...)
			continue
		}

		expectedByName := make(map[string]github.Repository, len(ownerInventory.Repositories))
		expectedSources := make(map[string]string, len(ownerInventory.Repositories))
		for _, repo := range ownerInventory.Repositories {
			key := strings.ToLower(repo.Name)
			expectedByName[key] = repo
			expectedSources[key] = repo.CloneURL
		}

		giteaByName := make(map[string]gitea.Repository, len(giteaInventory.Repositories))
		for _, repo := range giteaInventory.Repositories {
			giteaByName[strings.ToLower(repo.Name)] = repo
		}

		for _, repo := range ownerInventory.Repositories {
			key := strings.ToLower(repo.Name)
			giteaRepo, exists := giteaByName[key]
			if !exists {
				entries = append(entries, missingMirrorEntry(command, ownerInventory.Owner, repo, giteaInventory.Owner.Name))
				continue
			}
			giteaRepo.SourceVerification = gitea.ClassifySource(giteaRepo, expectedSources[key])
			entries = append(entries, classifyExistingMirror(command, ownerInventory.Owner, repo, giteaInventory.Owner.Name, giteaRepo))
		}

		if command != CommandPlan {
			for _, repo := range giteaInventory.Repositories {
				if _, expected := expectedByName[strings.ToLower(repo.Name)]; expected {
					continue
				}
				entries = append(entries, githubSourceMissingEntry(command, giteaInventory.Owner.Name, repo))
			}
		}

		entries = append(entries, skippedForkEntries(ownerInventory)...)
		if names := duplicateRepos[ownerKey]; len(names) > 0 {
			for _, repo := range giteaInventory.Repositories {
				if !names[strings.ToLower(repo.Name)] {
					continue
				}
				if hasDirectBlocker(entries, ownerInventory.Owner.Login, repo.Name) {
					continue
				}
				entries = append(entries, Entry{
					Status:          StatusWarnDuplicateName,
					Severity:        SeverityWarning,
					Owner:           giteaInventory.Owner.Name,
					Repository:      repo.Name,
					GiteaOwner:      giteaInventory.Owner.Name,
					GiteaRepository: repo.Name,
					Message:         "repository name also appears under another configured Gitea owner",
				})
			}
		}
	}

	return entries
}

func ErrorReport(command Command, startedAt time.Time, giteaBaseURL string, entry Entry) Report {
	finishedAt := time.Now().UTC()
	entry.Severity = SeverityFor(entry.Status)
	return Report{
		Command:      command,
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
		GiteaBaseURL: redact.SafeURL(giteaBaseURL),
		Results:      []Entry{entry},
		Summary:      Summarize([]Entry{entry}),
	}
}

func NewReport(command Command, startedAt time.Time, giteaBaseURL string, entries []Entry) Report {
	return Report{
		Command:      command,
		StartedAt:    startedAt,
		FinishedAt:   time.Now().UTC(),
		GiteaBaseURL: redact.SafeURL(giteaBaseURL),
		Summary:      Summarize(entries),
		Results:      entries,
	}
}

func Summarize(entries []Entry) Summary {
	summary := Summary{Total: len(entries), ByStatus: make(map[Status]int)}
	for _, entry := range entries {
		status := entry.Status
		summary.ByStatus[status]++
		switch status {
		case StatusCreatedMirror:
			summary.Created++
		}
		switch SeverityFor(status) {
		case SeverityOK:
			summary.OK++
		case SeverityAction:
			summary.Actions++
		case SeveritySkipped:
			summary.Skipped++
		case SeverityWarning:
			summary.Warnings++
		case SeverityBlocked:
			summary.Blocked++
		case SeverityError:
			summary.Errors++
		}
	}
	return summary
}

func SeverityFor(status Status) Severity {
	switch status {
	case StatusOKMirrored:
		return SeverityOK
	case StatusCreateMirror, StatusCreatedMirror:
		return SeverityAction
	case StatusSkippedFork:
		return SeveritySkipped
	case StatusWarnMirrorSourceUnverified, StatusWarnDuplicateName, StatusReportGitHubSourceMissing:
		return SeverityWarning
	case StatusBlockedMissingGiteaOwner, StatusBlockedOwnerTypeMismatch, StatusBlockedExistingNonMirror, StatusBlockedMirrorSourceMismatch:
		return SeverityBlocked
	default:
		return SeverityError
	}
}

func ExitCode(summary Summary) int {
	if summary.Errors > 0 || summary.Blocked > 0 {
		return 1
	}
	return 0
}

func ExpectedGiteaOwnerType(ownerType github.OwnerType) gitea.OwnerType {
	switch ownerType {
	case github.OwnerOrganization:
		return gitea.OwnerTypeOrganization
	case github.OwnerUser:
		return gitea.OwnerTypeUser
	default:
		return gitea.OwnerTypeUnknown
	}
}

func SortStatusKeys(counts map[Status]int) []Status {
	keys := make([]Status, 0, len(counts))
	for status := range counts {
		keys = append(keys, status)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})
	return keys
}

func SanitizeError(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	value := redact.Secrets(err.Error(), secrets...)
	return redact.SafeText(value)
}

func ownerFailureEntry(failure OwnerFailure, owner github.OwnerIdentity) Entry {
	status := failure.Status
	if status == "" {
		status = StatusErrorGiteaAccess
	}
	message := failure.Message
	if message == "" {
		message = "Gitea owner inventory failed"
	}
	return Entry{
		Status:      status,
		Severity:    SeverityFor(status),
		Owner:       owner.Login,
		GitHubOwner: owner.Login,
		GiteaOwner:  failure.Owner,
		Message:     message,
		Error:       SanitizeError(failure.Err),
	}
}

func missingMirrorEntry(command Command, owner github.OwnerIdentity, repo github.Repository, giteaOwner string) Entry {
	message := "expected Gitea pull mirror is missing"
	switch command {
	case CommandPlan:
		message = "would create Gitea pull mirror"
	case CommandSync:
		message = "missing Gitea pull mirror; sync will attempt creation"
	}
	return Entry{
		Status:           StatusCreateMirror,
		Severity:         SeverityAction,
		Owner:            owner.Login,
		Repository:       repo.Name,
		GitHubOwner:      owner.Login,
		GitHubRepository: repo.Name,
		GitHubFullName:   repo.FullName,
		GiteaOwner:       giteaOwner,
		GiteaRepository:  repo.Name,
		Private:          repo.Private,
		Archived:         repo.Archived,
		ExpectedSource:   redact.SafeURL(repo.CloneURL),
		Message:          message,
	}
}

func classifyExistingMirror(command Command, owner github.OwnerIdentity, ghRepo github.Repository, giteaOwner string, giteaRepo gitea.Repository) Entry {
	entry := Entry{
		Owner:            owner.Login,
		Repository:       ghRepo.Name,
		GitHubOwner:      owner.Login,
		GitHubRepository: ghRepo.Name,
		GitHubFullName:   ghRepo.FullName,
		GiteaOwner:       giteaOwner,
		GiteaRepository:  giteaRepo.Name,
		Private:          ghRepo.Private,
		Archived:         ghRepo.Archived,
		ExpectedSource:   redact.SafeURL(ghRepo.CloneURL),
		ObservedSource:   redact.SafeURL(giteaRepo.OriginalURL),
	}
	if !giteaRepo.Mirror {
		entry.Status = StatusBlockedExistingNonMirror
		entry.Severity = SeverityBlocked
		entry.Message = existingNonMirrorMessage(command)
		return entry
	}
	switch giteaRepo.SourceVerification {
	case gitea.SourceVerified:
		entry.Status = StatusOKMirrored
		entry.Severity = SeverityOK
		entry.Message = verifiedMirrorMessage(command)
	case gitea.SourceMismatch:
		entry.Status = StatusBlockedMirrorSourceMismatch
		entry.Severity = SeverityBlocked
		entry.Message = sourceMismatchMessage(command)
	case gitea.SourceUnverified:
		entry.Status = StatusWarnMirrorSourceUnverified
		entry.Severity = SeverityWarning
		entry.Message = sourceUnverifiedMessage(command)
	default:
		entry.Status = StatusWarnMirrorSourceUnverified
		entry.Severity = SeverityWarning
		entry.Message = sourceUnavailableMessage(command)
	}
	return entry
}

func githubSourceMissingEntry(command Command, ownerName string, repo gitea.Repository) Entry {
	message := "Gitea repository has no visible same-owner GitHub source in the configured inventory"
	if command == CommandSync {
		message = "Gitea repository has no visible same-owner GitHub source; no mutation attempted"
	}
	return Entry{
		Status:          StatusReportGitHubSourceMissing,
		Severity:        SeverityWarning,
		Owner:           ownerName,
		Repository:      repo.Name,
		GiteaOwner:      ownerName,
		GiteaRepository: repo.Name,
		ObservedSource:  redact.SafeURL(repo.OriginalURL),
		Message:         message,
	}
}

func existingNonMirrorMessage(command Command) string {
	switch command {
	case CommandPlan:
		return "sync cannot create because the destination repository exists and is not a pull mirror"
	case CommandVerify:
		return "expected Gitea repository exists but is not a pull mirror"
	default:
		return "skipped because the destination repository exists and is not a pull mirror"
	}
}

func verifiedMirrorMessage(command Command) string {
	switch command {
	case CommandPlan:
		return "no sync action needed; Gitea pull mirror source matches GitHub repository"
	case CommandVerify:
		return "existing expected Gitea pull mirror source matches GitHub repository"
	default:
		return "Gitea pull mirror already matches GitHub repository"
	}
}

func sourceMismatchMessage(command Command) string {
	switch command {
	case CommandPlan:
		return "sync cannot create because the existing pull mirror points at a different source"
	case CommandVerify:
		return "existing Gitea pull mirror source does not match GitHub repository"
	default:
		return "skipped because the existing pull mirror points at a different source"
	}
}

func sourceUnverifiedMessage(command Command) string {
	switch command {
	case CommandPlan:
		return "existing pull mirror prevents create; source could not be verified"
	case CommandVerify:
		return "existing Gitea pull mirror source could not be verified"
	default:
		return "existing pull mirror skipped because source could not be verified"
	}
}

func sourceUnavailableMessage(command Command) string {
	switch command {
	case CommandPlan:
		return "existing pull mirror prevents create; source verification is unavailable"
	case CommandVerify:
		return "existing Gitea pull mirror source verification is unavailable"
	default:
		return "existing pull mirror skipped because source verification is unavailable"
	}
}

func skippedForkEntries(ownerInventory github.OwnerInventory) []Entry {
	entries := make([]Entry, 0, len(ownerInventory.SkippedForks))
	for _, repo := range ownerInventory.SkippedForks {
		entries = append(entries, Entry{
			Status:           StatusSkippedFork,
			Severity:         SeveritySkipped,
			Owner:            ownerInventory.Owner.Login,
			Repository:       repo.Name,
			GitHubOwner:      ownerInventory.Owner.Login,
			GitHubRepository: repo.Name,
			GitHubFullName:   repo.FullName,
			Private:          repo.Private,
			Archived:         repo.Archived,
			ExpectedSource:   redact.SafeURL(repo.CloneURL),
			Message:          "GitHub fork repositories are not mirrored by this version",
		})
	}
	return entries
}

func duplicateGiteaRepositoryNames(giteaInventories map[string]gitea.OwnerInventory) map[string]map[string]bool {
	ownersByRepo := make(map[string]map[string]bool)
	for ownerKey, inventory := range giteaInventories {
		for _, repo := range inventory.Repositories {
			repoKey := strings.ToLower(repo.Name)
			if ownersByRepo[repoKey] == nil {
				ownersByRepo[repoKey] = make(map[string]bool)
			}
			ownersByRepo[repoKey][ownerKey] = true
		}
	}
	duplicates := make(map[string]map[string]bool)
	for repoKey, owners := range ownersByRepo {
		if len(owners) < 2 {
			continue
		}
		for ownerKey := range owners {
			if duplicates[ownerKey] == nil {
				duplicates[ownerKey] = make(map[string]bool)
			}
			duplicates[ownerKey][repoKey] = true
		}
	}
	return duplicates
}

func hasDirectBlocker(entries []Entry, owner string, repo string) bool {
	for _, entry := range entries {
		if !strings.EqualFold(entry.GitHubOwner, owner) || !strings.EqualFold(entry.GitHubRepository, repo) {
			continue
		}
		if SeverityFor(entry.Status) == SeverityBlocked {
			return true
		}
	}
	return false
}

func MarkCreated(entry Entry, repository gitea.Repository) Entry {
	entry.Status = StatusCreatedMirror
	entry.Severity = SeverityAction
	entry.GiteaRepository = repository.Name
	entry.ObservedSource = redact.SafeURL(repository.OriginalURL)
	entry.Message = "created Gitea pull mirror and confirmed repository exists"
	return entry
}

func MarkSyncFailed(entry Entry, status Status, err error, secrets ...string) Entry {
	entry.Status = status
	entry.Severity = SeverityError
	entry.Message = "failed to create and confirm Gitea pull mirror"
	entry.Error = SanitizeError(err, secrets...)
	return entry
}

func ValidateCreatedMirror(repository gitea.Repository) error {
	if !repository.Mirror {
		return fmt.Errorf("created repository %s is not reported as a mirror", repository.FullName)
	}
	if repository.SourceVerification == gitea.SourceMismatch {
		return fmt.Errorf("created repository %s reports a mismatched mirror source", repository.FullName)
	}
	return nil
}
