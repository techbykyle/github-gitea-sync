package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github-gitea-sync/internal/redact"
)

const (
	defaultBaseURL   = "https://api.github.com"
	defaultUserAgent = "github-gitea-sync/phase-002"
	perPage          = 100
)

type OwnerType string

const (
	OwnerUser         OwnerType = "User"
	OwnerOrganization OwnerType = "Organization"
)

type OwnerIdentity struct {
	Configured string
	Login      string
	Type       OwnerType
}

type Repository struct {
	ID       int64
	Owner    string
	Name     string
	FullName string
	Private  bool
	Archived bool
	Fork     bool
	HTMLURL  string
	CloneURL string
	SSHURL   string
}

type OwnerInventory struct {
	Owner        OwnerIdentity
	Repositories []Repository
	SkippedForks []Repository
}

type ConditionalMetadata struct {
	ETag           string
	LastObservedAt time.Time
}

type RepositoryPage struct {
	Repositories []Repository
	Full         bool
}

type InventoryRequest struct {
	Owners              []string
	IncludePrivate      bool
	IncludeArchived     bool
	CachedOwners        map[string]OwnerIdentity
	ConditionalMetadata map[string]ConditionalMetadata
	CachedPages         map[string]RepositoryPage
}

type InventoryResult struct {
	Owners              []OwnerInventory
	ConditionalMetadata map[string]ConditionalMetadata
	CachedPages         map[string]RepositoryPage
}

type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
	userAgent  string
}

type Error struct {
	Code       string
	Owner      string
	Operation  string
	StatusCode int
	Detail     string
	RetryAfter time.Duration
	RetryAt    time.Time
}

func (e *Error) Error() string {
	var parts []string
	parts = append(parts, e.Code)
	if e.Owner != "" {
		parts = append(parts, "owner="+e.Owner)
	}
	if e.Operation != "" {
		parts = append(parts, "operation="+e.Operation)
	}
	if e.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if e.RetryAfter > 0 {
		parts = append(parts, "retry_after="+e.RetryAfter.String())
	}
	if !e.RetryAt.IsZero() {
		parts = append(parts, "retry_at="+e.RetryAt.Format(time.RFC3339))
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	return strings.Join(parts, ": ")
}

func NewClient(token string) *Client {
	return &Client{
		httpClient: http.DefaultClient,
		baseURL:    defaultBaseURL,
		token:      token,
		userAgent:  defaultUserAgent,
	}
}

func (c *Client) Inventory(ctx context.Context, req InventoryRequest) (InventoryResult, error) {
	result := InventoryResult{
		Owners:              make([]OwnerInventory, 0, len(req.Owners)),
		ConditionalMetadata: make(map[string]ConditionalMetadata),
		CachedPages:         make(map[string]RepositoryPage),
	}

	for _, configuredOwner := range req.Owners {
		owner, ownerConditionals, err := c.detectOwner(ctx, configuredOwner, req)
		if err != nil {
			return InventoryResult{}, err
		}

		repositories, pageConditionals, pageSnapshots, err := c.listOwnerRepositories(ctx, owner, req)
		if err != nil {
			return InventoryResult{}, err
		}

		ownerInventory := OwnerInventory{Owner: owner}
		for _, repository := range repositories {
			if !req.IncludePrivate && repository.Private {
				continue
			}
			if !req.IncludeArchived && repository.Archived {
				continue
			}
			if repository.Fork {
				ownerInventory.SkippedForks = append(ownerInventory.SkippedForks, repository)
				continue
			}
			ownerInventory.Repositories = append(ownerInventory.Repositories, repository)
		}

		result.Owners = append(result.Owners, ownerInventory)
		for key, metadata := range ownerConditionals {
			result.ConditionalMetadata[key] = metadata
		}
		for key, metadata := range pageConditionals {
			result.ConditionalMetadata[key] = metadata
		}
		for key, page := range pageSnapshots {
			result.CachedPages[key] = page
		}
	}

	return result, nil
}

func (c *Client) detectOwner(ctx context.Context, configuredOwner string, req InventoryRequest) (OwnerIdentity, map[string]ConditionalMetadata, error) {
	operation := "detect_owner"
	key := requestKey(operation, configuredOwner, "")
	conditionals := make(map[string]ConditionalMetadata)

	headers := map[string]string{}
	cachedOwner, hasCachedOwner := req.CachedOwners[strings.ToLower(configuredOwner)]
	if metadata, ok := req.ConditionalMetadata[key]; ok && metadata.ETag != "" && hasCachedOwner {
		headers["If-None-Match"] = metadata.ETag
	}

	var payload ownerPayload
	resp, err := c.getJSON(ctx, configuredOwner, operation, "/users/"+url.PathEscape(configuredOwner), nil, headers, &payload)
	if err != nil {
		return OwnerIdentity{}, nil, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return cachedOwner, conditionals, nil
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		conditionals[key] = ConditionalMetadata{
			ETag:           etag,
			LastObservedAt: time.Now().UTC(),
		}
	}

	ownerType := OwnerType(payload.Type)
	if ownerType != OwnerUser && ownerType != OwnerOrganization {
		return OwnerIdentity{}, nil, &Error{
			Code:      "ERROR_GITHUB",
			Owner:     configuredOwner,
			Operation: operation,
			Detail:    "unsupported GitHub owner type " + payload.Type,
		}
	}
	return OwnerIdentity{
		Configured: configuredOwner,
		Login:      payload.Login,
		Type:       ownerType,
	}, conditionals, nil
}

func (c *Client) listOwnerRepositories(ctx context.Context, owner OwnerIdentity, req InventoryRequest) ([]Repository, map[string]ConditionalMetadata, map[string]RepositoryPage, error) {
	switch owner.Type {
	case OwnerOrganization:
		return c.listRepositories(ctx, owner, "list_org_repositories", "/orgs/"+url.PathEscape(owner.Login)+"/repos", map[string]string{"type": "all"}, req)
	case OwnerUser:
		publicRepos, publicConditionals, publicSnapshots, err := c.listRepositories(ctx, owner, "list_user_public_repositories", "/users/"+url.PathEscape(owner.Login)+"/repos", map[string]string{
			"type": "owner",
		}, req)
		if err != nil {
			return nil, nil, nil, err
		}
		permissionedRepos, permissionedConditionals, permissionedSnapshots, err := c.listRepositories(ctx, owner, "list_user_repositories", "/user/repos", map[string]string{
			"visibility":  "all",
			"affiliation": "owner,collaborator,organization_member",
		}, req)
		if err != nil {
			return nil, nil, nil, err
		}
		repositories := mergeRepositories(publicRepos, permissionedRepos)
		conditionals := mergeConditionalMetadata(publicConditionals, permissionedConditionals)
		snapshots := mergePageSnapshots(publicSnapshots, permissionedSnapshots)
		return repositories, conditionals, snapshots, nil
	default:
		return nil, nil, nil, &Error{
			Code:      "ERROR_GITHUB",
			Owner:     owner.Configured,
			Operation: "list_repositories",
			Detail:    "unsupported GitHub owner type " + string(owner.Type),
		}
	}
}

func (c *Client) listRepositories(ctx context.Context, owner OwnerIdentity, operation string, path string, baseQuery map[string]string, req InventoryRequest) ([]Repository, map[string]ConditionalMetadata, map[string]RepositoryPage, error) {
	var repositories []Repository
	conditionals := make(map[string]ConditionalMetadata)
	pageSnapshots := make(map[string]RepositoryPage)

	for page := 1; ; page++ {
		pageKey := requestKey(operation, owner.Login, strconv.Itoa(page))
		query := make(map[string]string, len(baseQuery)+2)
		for key, value := range baseQuery {
			query[key] = value
		}
		query["per_page"] = strconv.Itoa(perPage)
		query["page"] = strconv.Itoa(page)

		headers := map[string]string{}
		cachedPage, hasCachedPage := req.CachedPages[pageKey]
		if metadata, ok := req.ConditionalMetadata[pageKey]; ok && metadata.ETag != "" && hasCachedPage {
			headers["If-None-Match"] = metadata.ETag
		}

		var payload []repositoryPayload
		resp, err := c.getJSON(ctx, owner.Configured, operation, path, query, headers, &payload)
		if err != nil {
			return nil, nil, nil, err
		}
		var pageRepositories []Repository
		pageFull := false
		if resp.StatusCode == http.StatusNotModified {
			if !hasCachedPage {
				return nil, nil, nil, &Error{
					Code:      "ERROR_GITHUB",
					Owner:     owner.Configured,
					Operation: operation,
					Detail:    "repository list returned 304 without a complete cached page snapshot",
				}
			}
			pageRepositories = cachedPage.Repositories
			pageFull = cachedPage.Full
		} else {
			if etag := resp.Header.Get("ETag"); etag != "" {
				conditionals[pageKey] = ConditionalMetadata{
					ETag:           etag,
					LastObservedAt: time.Now().UTC(),
				}
			}

			for _, repo := range payload {
				if !strings.EqualFold(repo.Owner.Login, owner.Login) {
					continue
				}
				pageRepositories = append(pageRepositories, repo.toRepository())
			}
			pageFull = len(payload) == perPage
			pageSnapshots[pageKey] = RepositoryPage{
				Repositories: pageRepositories,
				Full:         pageFull,
			}
		}

		for _, repo := range pageRepositories {
			if !strings.EqualFold(repo.Owner, owner.Login) {
				return nil, nil, nil, &Error{
					Code:      "ERROR_GITHUB",
					Owner:     owner.Configured,
					Operation: operation,
					Detail:    "cached repository page includes a repository for a different owner",
				}
			}
			repositories = append(repositories, repo)
		}
		if !pageFull {
			break
		}
		if resp.StatusCode == http.StatusNotModified {
			if _, ok := req.CachedPages[requestKey(operation, owner.Login, strconv.Itoa(page+1))]; !ok {
				return nil, nil, nil, &Error{
					Code:      "ERROR_GITHUB",
					Owner:     owner.Configured,
					Operation: operation,
					Detail:    "repository list cache is incomplete for a 304 response",
				}
			}
		}
	}

	return repositories, conditionals, pageSnapshots, nil
}

func mergeRepositories(groups ...[]Repository) []Repository {
	var repositories []Repository
	seen := make(map[int64]struct{})
	for _, group := range groups {
		for _, repository := range group {
			if _, ok := seen[repository.ID]; ok {
				continue
			}
			seen[repository.ID] = struct{}{}
			repositories = append(repositories, repository)
		}
	}
	return repositories
}

func mergeConditionalMetadata(groups ...map[string]ConditionalMetadata) map[string]ConditionalMetadata {
	merged := make(map[string]ConditionalMetadata)
	for _, group := range groups {
		for key, metadata := range group {
			merged[key] = metadata
		}
	}
	return merged
}

func mergePageSnapshots(groups ...map[string]RepositoryPage) map[string]RepositoryPage {
	merged := make(map[string]RepositoryPage)
	for _, group := range groups {
		for key, page := range group {
			merged[key] = page
		}
	}
	return merged
}

type responseInfo struct {
	StatusCode int
	Header     http.Header
}

func (c *Client) getJSON(ctx context.Context, owner string, operation string, path string, query map[string]string, headers map[string]string, target any) (responseInfo, error) {
	requestURL, err := c.requestURL(path, query)
	if err != nil {
		return responseInfo{}, &Error{Code: "ERROR_GITHUB", Owner: owner, Operation: operation, Detail: err.Error()}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return responseInfo{}, &Error{Code: "ERROR_GITHUB", Owner: owner, Operation: operation, Detail: redact.Secrets(err.Error(), c.token)}
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", c.userAgent)
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return responseInfo{}, &Error{Code: "ERROR_GITHUB", Owner: owner, Operation: operation, Detail: redact.Secrets(err.Error(), c.token)}
	}
	defer response.Body.Close()

	info := responseInfo{StatusCode: response.StatusCode, Header: response.Header.Clone()}
	if response.StatusCode == http.StatusNotModified {
		return info, nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return info, c.responseError(owner, operation, response)
	}
	if target == nil {
		return info, nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return info, &Error{
			Code:       "ERROR_GITHUB",
			Owner:      owner,
			Operation:  operation,
			StatusCode: response.StatusCode,
			Detail:     "decode response: " + redact.Secrets(err.Error(), c.token),
		}
	}
	return info, nil
}

func (c *Client) responseError(owner string, operation string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	detail := strings.TrimSpace(redact.Secrets(string(body), c.token))
	if detail == "" {
		detail = http.StatusText(response.StatusCode)
	}

	if response.StatusCode == http.StatusForbidden && response.Header.Get("X-RateLimit-Remaining") == "0" {
		return &Error{
			Code:       "ERROR_RATE_LIMITED",
			Owner:      owner,
			Operation:  operation,
			StatusCode: response.StatusCode,
			Detail:     detail,
			RetryAt:    parseRateLimitReset(response.Header.Get("X-RateLimit-Reset")),
		}
	}
	if retryAfter := parseRetryAfter(response.Header.Get("Retry-After")); retryAfter > 0 || isSecondaryLimit(response.StatusCode, detail) {
		return &Error{
			Code:       "ERROR_RATE_LIMITED",
			Owner:      owner,
			Operation:  operation,
			StatusCode: response.StatusCode,
			Detail:     detail,
			RetryAfter: retryAfter,
		}
	}

	return &Error{
		Code:       "ERROR_GITHUB",
		Owner:      owner,
		Operation:  operation,
		StatusCode: response.StatusCode,
		Detail:     detail,
	}
}

func (c *Client) requestURL(path string, query map[string]string) (string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	relative, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	resolved := base.ResolveReference(relative)
	values := resolved.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	resolved.RawQuery = values.Encode()
	return resolved.String(), nil
}

func parseRateLimitReset(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		until := time.Until(retryAt)
		if until > 0 {
			return until
		}
	}
	return 0
}

func isSecondaryLimit(status int, detail string) bool {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return false
	}
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "secondary rate limit") ||
		strings.Contains(lower, "abuse detection") ||
		strings.Contains(lower, "too many requests")
}

func requestKey(operation string, owner string, page string) string {
	parts := []string{"github", operation, strings.ToLower(owner)}
	if page != "" {
		parts = append(parts, "page", page)
	}
	return strings.Join(parts, ":")
}

type ownerPayload struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type repositoryPayload struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	Fork     bool   `json:"fork"`
	HTMLURL  string `json:"html_url"`
	CloneURL string `json:"clone_url"`
	SSHURL   string `json:"ssh_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (r repositoryPayload) toRepository() Repository {
	return Repository{
		ID:       r.ID,
		Owner:    r.Owner.Login,
		Name:     r.Name,
		FullName: r.FullName,
		Private:  r.Private,
		Archived: r.Archived,
		Fork:     r.Fork,
		HTMLURL:  redact.SafeURL(r.HTMLURL),
		CloneURL: redact.SafeURL(r.CloneURL),
		SSHURL:   redact.SafeURL(r.SSHURL),
	}
}
