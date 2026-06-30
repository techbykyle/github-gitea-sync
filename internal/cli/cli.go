package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github-gitea-sync/internal/config"
	"github-gitea-sync/internal/gitea"
	"github-gitea-sync/internal/github"
	"github-gitea-sync/internal/output"
	"github-gitea-sync/internal/reconcile"
	"github-gitea-sync/internal/redact"
	"github-gitea-sync/internal/state"
	"github-gitea-sync/internal/version"
)

const (
	exitOK     = 0
	exitError  = 1
	exitUsage  = 2
	exitConfig = 3
)

func Run(args []string, stdout io.Writer, stderr io.Writer, lookup config.LookupEnv) int {
	if len(args) == 0 {
		printHelp(stdout)
		return exitOK
	}

	switch args[0] {
	case "help", "-h", "--help":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "ERROR_USAGE: help does not accept arguments\n")
			return exitUsage
		}
		printHelp(stdout)
		return exitOK
	case "version":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "ERROR_USAGE: version does not accept arguments\n")
			return exitUsage
		}
		fmt.Fprintf(stdout, "%s %s\n", version.Name, version.Version)
		return exitOK
	case "plan", "sync", "verify":
		return runReconciliationCommand(reconcile.Command(args[0]), args[1:], stdout, stderr, lookup)
	default:
		fmt.Fprintf(stderr, "ERROR_USAGE: unknown command %q\n\n", args[0])
		printHelp(stderr)
		return exitUsage
	}
}

func runReconciliationCommand(command reconcile.Command, args []string, stdout io.Writer, stderr io.Writer, lookup config.LookupEnv) int {
	startedAt := time.Now().UTC()
	mode, ok := parseCommandFlags(command, args, stderr)
	if !ok {
		return exitUsage
	}

	cfg, err := config.Load(lookup)
	if err != nil {
		return emitErrorReport(command, mode, stdout, stderr, startedAt, "", reconcile.Entry{
			Status:  reconcile.StatusErrorConfig,
			Message: "invalid configuration",
			Error:   reconcile.SanitizeError(err),
		}, exitConfig)
	}

	localState, statePath, err := state.Load(cfg.StatePath)
	if err != nil {
		return emitErrorReport(command, mode, stdout, stderr, startedAt, cfg.GiteaBaseURL, reconcile.Entry{
			Status:  reconcile.StatusErrorInternal,
			Message: "failed to load local state",
			Error:   reconcile.SanitizeError(err, cfg.GitHubToken, cfg.GiteaToken),
		}, exitError)
	}

	giteaClient, err := gitea.NewClient(cfg.GiteaBaseURL, cfg.GiteaToken)
	if err != nil {
		return emitErrorReport(command, mode, stdout, stderr, startedAt, cfg.GiteaBaseURL, reconcile.Entry{
			Status:  reconcile.StatusErrorGiteaAccess,
			Message: "failed to initialize Gitea client",
			Error:   reconcile.SanitizeError(err, cfg.GitHubToken, cfg.GiteaToken),
		}, exitError)
	}

	ctx := context.Background()
	githubClient := github.NewClient(cfg.GitHubToken)
	inventory, err := githubClient.Inventory(ctx, github.InventoryRequest{
		Owners:              cfg.GitHubAccounts,
		IncludePrivate:      cfg.IncludePrivate,
		IncludeArchived:     cfg.IncludeArchived,
		CachedOwners:        cachedOwners(localState),
		ConditionalMetadata: cachedConditionalMetadata(localState),
		CachedPages:         cachedRepositoryPages(localState),
	})
	if err != nil {
		status := reconcile.StatusErrorGitHubAccess
		var githubErr *github.Error
		if errors.As(err, &githubErr) && githubErr.Code == "ERROR_RATE_LIMITED" {
			status = reconcile.StatusErrorRateLimited
		}
		return emitErrorReport(command, mode, stdout, stderr, startedAt, cfg.GiteaBaseURL, reconcile.Entry{
			Status:      status,
			Owner:       githubErrorOwner(err),
			GitHubOwner: githubErrorOwner(err),
			Message:     "failed to inventory GitHub repositories",
			Error:       reconcile.SanitizeError(err, cfg.GitHubToken, cfg.GiteaToken),
		}, exitError)
	}

	giteaInventories, ownerFailures := inventoryGiteaOwners(ctx, giteaClient, inventory)
	entries := reconcile.Build(command, inventory, giteaInventories, ownerFailures)
	if command == reconcile.CommandSync {
		entries = runSync(ctx, giteaClient, cfg, entries)
	}

	if canSaveState(entries) {
		observedAt := time.Now().UTC()
		applyInventory(&localState, inventory, observedAt)
		if err := state.Save(statePath, localState, cfg.GitHubToken, cfg.GiteaToken); err != nil {
			entries = append(entries, reconcile.Entry{
				Status:   reconcile.StatusErrorInternal,
				Severity: reconcile.SeverityError,
				Message:  "failed to save local state",
				Error:    reconcile.SanitizeError(err, cfg.GitHubToken, cfg.GiteaToken),
			})
		}
	}

	report := reconcile.NewReport(command, startedAt, cfg.GiteaBaseURL, entries)
	if err := output.Write(stdout, mode, report); err != nil {
		fmt.Fprintf(stderr, "ERROR_INTERNAL: %s\n", redact.Secrets(err.Error(), cfg.GitHubToken, cfg.GiteaToken))
		return exitError
	}
	return reconcile.ExitCode(report.Summary)
}

func inventoryGiteaOwners(ctx context.Context, client *gitea.Client, inventory github.InventoryResult) (map[string]gitea.OwnerInventory, map[string]reconcile.OwnerFailure) {
	inventories := make(map[string]gitea.OwnerInventory, len(inventory.Owners))
	failures := make(map[string]reconcile.OwnerFailure)
	for _, ownerInventory := range inventory.Owners {
		ownerName := ownerInventory.Owner.Login
		ownerKey := strings.ToLower(ownerName)
		owner, err := client.ValidateOwner(ctx, gitea.OwnerValidationRequest{
			Name:         ownerName,
			ExpectedType: reconcile.ExpectedGiteaOwnerType(ownerInventory.Owner.Type),
		})
		if err != nil {
			failures[ownerKey] = giteaOwnerFailure(ownerName, err)
			continue
		}
		repositories, err := client.ListOwnerRepositories(ctx, owner)
		if err != nil {
			failures[ownerKey] = giteaOwnerFailure(ownerName, err)
			continue
		}
		inventories[ownerKey] = gitea.OwnerInventory{
			Owner:        owner,
			Repositories: repositories,
		}
	}
	return inventories, failures
}

func giteaOwnerFailure(owner string, err error) reconcile.OwnerFailure {
	status := reconcile.StatusErrorGiteaAccess
	message := "failed to inventory Gitea owner"
	var giteaErr *gitea.Error
	if errors.As(err, &giteaErr) {
		switch giteaErr.Code {
		case "ERROR_GITEA_OWNER_MISSING":
			status = reconcile.StatusBlockedMissingGiteaOwner
			message = "Gitea owner does not exist"
		case "ERROR_GITEA_OWNER_TYPE":
			status = reconcile.StatusBlockedOwnerTypeMismatch
			message = "Gitea owner type does not match GitHub owner type"
		}
	}
	return reconcile.OwnerFailure{
		Owner:   owner,
		Status:  status,
		Message: message,
		Err:     err,
	}
}

func runSync(ctx context.Context, client *gitea.Client, cfg config.Config, entries []reconcile.Entry) []reconcile.Entry {
	for index, entry := range entries {
		if entry.Status != reconcile.StatusCreateMirror {
			continue
		}
		created, err := client.CreatePullMirror(ctx, gitea.CreatePullMirrorRequest{
			Owner:          entry.GiteaOwner,
			Name:           entry.GiteaRepository,
			CloneURL:       entry.ExpectedSource,
			Private:        entry.Private,
			MirrorInterval: cfg.MirrorInterval,
			GitHubToken:    cfg.GitHubToken,
		})
		if err != nil {
			entries[index] = reconcile.MarkSyncFailed(entry, reconcile.StatusErrorGiteaAccess, err, cfg.GitHubToken, cfg.GiteaToken)
			continue
		}
		confirmed, err := client.GetRepository(ctx, entry.GiteaOwner, entry.GiteaRepository, entry.ExpectedSource)
		if err != nil {
			entries[index] = reconcile.MarkSyncFailed(entry, reconcile.StatusErrorGiteaAccess, err, cfg.GitHubToken, cfg.GiteaToken)
			continue
		}
		if err := reconcile.ValidateCreatedMirror(confirmed); err != nil {
			entries[index] = reconcile.MarkSyncFailed(entry, reconcile.StatusErrorInternal, err, cfg.GitHubToken, cfg.GiteaToken)
			continue
		}
		if confirmed.ID == 0 {
			confirmed = created
			confirmed.SourceVerification = gitea.ClassifySource(confirmed, entry.ExpectedSource)
		}
		entries[index] = reconcile.MarkCreated(entry, confirmed)
	}
	return entries
}

func canSaveState(entries []reconcile.Entry) bool {
	for _, entry := range entries {
		if reconcile.SeverityFor(entry.Status) == reconcile.SeverityError {
			return false
		}
	}
	return true
}

func emitErrorReport(command reconcile.Command, mode output.Mode, stdout io.Writer, stderr io.Writer, startedAt time.Time, giteaBaseURL string, entry reconcile.Entry, code int) int {
	report := reconcile.ErrorReport(command, startedAt, giteaBaseURL, entry)
	if err := output.Write(stdout, mode, report); err != nil {
		fmt.Fprintf(stderr, "ERROR_INTERNAL: %s\n", err)
		return exitError
	}
	return code
}

func githubErrorOwner(err error) string {
	var githubErr *github.Error
	if errors.As(err, &githubErr) {
		return githubErr.Owner
	}
	return ""
}

func cachedOwners(st state.State) map[string]github.OwnerIdentity {
	owners := make(map[string]github.OwnerIdentity, len(st.GitHub.Owners))
	for key, owner := range st.GitHub.Owners {
		owners[key] = github.OwnerIdentity{
			Configured: owner.Configured,
			Login:      owner.Login,
			Type:       github.OwnerType(owner.Type),
		}
	}
	return owners
}

func cachedConditionalMetadata(st state.State) map[string]github.ConditionalMetadata {
	metadata := make(map[string]github.ConditionalMetadata, len(st.GitHub.ConditionalMetadata))
	for key, item := range st.GitHub.ConditionalMetadata {
		metadata[key] = github.ConditionalMetadata{
			ETag:           item.ETag,
			LastObservedAt: item.LastObservedAt,
		}
	}
	return metadata
}

func cachedRepositoryPages(st state.State) map[string]github.RepositoryPage {
	pages := make(map[string]github.RepositoryPage, len(st.GitHub.RepositoryPages))
	for key, page := range st.GitHub.RepositoryPages {
		repositories := make([]github.Repository, 0, len(page.Repositories))
		for _, repo := range page.Repositories {
			repositories = append(repositories, githubRepository(repo))
		}
		pages[key] = github.RepositoryPage{
			Repositories: repositories,
			Full:         page.Full,
		}
	}
	return pages
}

func applyInventory(st *state.State, inventory github.InventoryResult, observedAt time.Time) {
	if st.GitHub.Owners == nil {
		st.GitHub.Owners = make(map[string]state.GitHubOwnerState)
	}
	if st.GitHub.RepositoriesByID == nil {
		st.GitHub.RepositoriesByID = make(map[string]state.GitHubRepository)
	}
	if st.GitHub.ConditionalMetadata == nil {
		st.GitHub.ConditionalMetadata = make(map[string]state.ConditionalMetadata)
	}
	if st.GitHub.RepositoryPages == nil {
		st.GitHub.RepositoryPages = make(map[string]state.GitHubRepositoryPage)
	}

	st.GitHub.LastSuccessfulInventoryAt = &observedAt
	for _, owner := range inventory.Owners {
		ownerState := state.GitHubOwnerState{
			Configured:                owner.Owner.Configured,
			Login:                     owner.Owner.Login,
			Type:                      string(owner.Owner.Type),
			LastSuccessfulInventoryAt: observedAt,
			Repositories:              make([]state.GitHubRepository, 0, len(owner.Repositories)),
			SkippedForks:              make([]state.GitHubRepository, 0, len(owner.SkippedForks)),
		}
		for _, repo := range owner.Repositories {
			snapshot := stateRepository(repo)
			ownerState.Repositories = append(ownerState.Repositories, snapshot)
			st.GitHub.RepositoriesByID[strconv.FormatInt(repo.ID, 10)] = snapshot
		}
		for _, repo := range owner.SkippedForks {
			snapshot := stateRepository(repo)
			ownerState.SkippedForks = append(ownerState.SkippedForks, snapshot)
			st.GitHub.RepositoriesByID[strconv.FormatInt(repo.ID, 10)] = snapshot
		}
		st.GitHub.Owners[strings.ToLower(owner.Owner.Configured)] = ownerState
		st.GitHub.Owners[strings.ToLower(owner.Owner.Login)] = ownerState
	}
	for key, item := range inventory.ConditionalMetadata {
		st.GitHub.ConditionalMetadata[key] = state.ConditionalMetadata{
			ETag:           item.ETag,
			LastObservedAt: item.LastObservedAt,
		}
	}
	for key, page := range inventory.CachedPages {
		repositories := make([]state.GitHubRepository, 0, len(page.Repositories))
		for _, repo := range page.Repositories {
			repositories = append(repositories, stateRepository(repo))
		}
		st.GitHub.RepositoryPages[key] = state.GitHubRepositoryPage{
			Repositories: repositories,
			Full:         page.Full,
		}
	}
}

func stateRepository(repo github.Repository) state.GitHubRepository {
	return state.GitHubRepository{
		ID:       repo.ID,
		Owner:    repo.Owner,
		Name:     repo.Name,
		FullName: repo.FullName,
		Private:  repo.Private,
		Archived: repo.Archived,
		Fork:     repo.Fork,
		HTMLURL:  repo.HTMLURL,
		CloneURL: repo.CloneURL,
		SSHURL:   repo.SSHURL,
	}
}

func githubRepository(repo state.GitHubRepository) github.Repository {
	return github.Repository{
		ID:       repo.ID,
		Owner:    repo.Owner,
		Name:     repo.Name,
		FullName: repo.FullName,
		Private:  repo.Private,
		Archived: repo.Archived,
		Fork:     repo.Fork,
		HTMLURL:  repo.HTMLURL,
		CloneURL: repo.CloneURL,
		SSHURL:   repo.SSHURL,
	}
}

func parseCommandFlags(command reconcile.Command, args []string, stderr io.Writer) (output.Mode, bool) {
	mode := output.ModeText
	for _, arg := range args {
		switch {
		case arg == "--json":
			mode = output.ModeJSON
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(stderr, "ERROR_USAGE: %s does not support flag %q\n", command, arg)
			return output.ModeText, false
		default:
			fmt.Fprintf(stderr, "ERROR_USAGE: %s does not accept positional arguments: %s\n", command, strings.Join(args, " "))
			return output.ModeText, false
		}
	}
	return mode, true
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  github-gitea-sync <command> [flags]

Commands:
  plan      Read GitHub and Gitea state and report planned mirror actions
  sync      Create only safe missing Gitea pull mirrors
  verify    Read GitHub and Gitea state and report mirror health
  help      Show this help
  version   Print version information

Flags:
  --json    Emit stable JSON output for plan, sync, and verify

Configuration:
  Reconciliation commands read configuration only from GGS_* environment variables.
`)
}
