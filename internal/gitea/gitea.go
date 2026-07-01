package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github-gitea-sync/internal/redact"
)

const (
	defaultUserAgent = "github-gitea-sync/phase-003"
	apiPrefix        = "/api/v1"
	pageLimit        = 100
)

type OwnerType string

const (
	OwnerTypeUnknown      OwnerType = "unknown"
	OwnerTypeUser         OwnerType = "user"
	OwnerTypeOrganization OwnerType = "organization"
)

type SourceVerification string

const (
	SourceNotApplicable SourceVerification = "not_applicable"
	SourceVerified      SourceVerification = "verified"
	SourceMismatch      SourceVerification = "mismatch"
	SourceUnverified    SourceVerification = "unverified"
)

type OwnerValidationRequest struct {
	Name         string
	ExpectedType OwnerType
}

type Owner struct {
	Configured    string
	Name          string
	ID            int64
	Type          OwnerType
	TypeVerified  bool
	Visibility    string
	HTMLURL       string
	Missing       bool
	TypeMismatch  bool
	ExpectedType  OwnerType
	ObservedAtUTC time.Time
}

type Repository struct {
	ID                 int64
	Owner              string
	Name               string
	FullName           string
	Private            bool
	Archived           bool
	Fork               bool
	HTMLURL            string
	CloneURL           string
	SSHURL             string
	Mirror             bool
	MirrorInterval     string
	MirrorUpdated      *time.Time
	OriginalURL        string
	SourceVerification SourceVerification
	ObservedAtUTC      time.Time
}

type OwnerInventory struct {
	Owner        Owner
	Repositories []Repository
}

type CreatePullMirrorRequest struct {
	Owner          string
	Name           string
	CloneURL       string
	Private        bool
	MirrorInterval time.Duration
	GitHubToken    string
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
	Repository string
	Operation  string
	StatusCode int
	Detail     string
}

func (e *Error) Error() string {
	var parts []string
	parts = append(parts, e.Code)
	if e.Owner != "" {
		parts = append(parts, "owner="+e.Owner)
	}
	if e.Repository != "" {
		parts = append(parts, "repository="+e.Repository)
	}
	if e.Operation != "" {
		parts = append(parts, "operation="+e.Operation)
	}
	if e.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	return strings.Join(parts, ": ")
}

func NewClient(baseURL string, token string) (*Client, error) {
	normalized, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, &Error{Code: "ERROR_GITEA_ACCESS", Operation: "configure_client", Detail: redact.Secrets(err.Error(), token)}
	}
	return &Client{
		httpClient: http.DefaultClient,
		baseURL:    normalized,
		token:      token,
		userAgent:  defaultUserAgent,
	}, nil
}

func (c *Client) ValidateOwner(ctx context.Context, req OwnerValidationRequest) (Owner, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return Owner{}, &Error{Code: "ERROR_GITEA_ACCESS", Operation: "validate_owner", Detail: "owner name must not be empty"}
	}

	var org organizationPayload
	_, err := c.getJSON(ctx, name, "", "get_organization", "/orgs/"+url.PathEscape(name), nil, &org)
	if err == nil {
		owner := c.ownerFromOrganization(req, org)
		if owner.TypeMismatch {
			return owner, &Error{Code: "ERROR_GITEA_OWNER_TYPE", Owner: req.Name, Operation: "validate_owner", Detail: "Gitea owner is an organization, expected user"}
		}
		return owner, nil
	}
	if !isStatus(err, http.StatusNotFound) {
		return Owner{}, err
	}

	var user userPayload
	userStatus, err := c.getJSON(ctx, name, "", "get_user", "/users/"+url.PathEscape(name), nil, &user)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return Owner{
				Configured:    name,
				Name:          name,
				Type:          OwnerTypeUnknown,
				TypeVerified:  true,
				Missing:       true,
				ExpectedType:  req.ExpectedType,
				ObservedAtUTC: time.Now().UTC(),
			}, &Error{Code: "ERROR_GITEA_OWNER_MISSING", Owner: name, Operation: "validate_owner", StatusCode: userStatus.StatusCode, Detail: "Gitea owner not found as organization or user"}
		}
		return Owner{}, err
	}
	return c.ownerFromUser(req, user)
}

func (c *Client) ListOwnerRepositories(ctx context.Context, owner Owner) ([]Repository, error) {
	switch owner.Type {
	case OwnerTypeOrganization:
		return c.listRepositories(ctx, owner, "list_organization_repositories", "/orgs/"+url.PathEscape(owner.Name)+"/repos")
	case OwnerTypeUser:
		return c.listRepositories(ctx, owner, "list_user_repositories", "/users/"+url.PathEscape(owner.Name)+"/repos")
	default:
		return nil, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner.Configured, Operation: "list_repositories", Detail: "owner type is not validated"}
	}
}

func (c *Client) InventoryOwner(ctx context.Context, req OwnerValidationRequest, expectedSources map[string]string) (OwnerInventory, error) {
	owner, err := c.ValidateOwner(ctx, req)
	if err != nil {
		return OwnerInventory{}, err
	}
	repositories, err := c.ListOwnerRepositories(ctx, owner)
	if err != nil {
		return OwnerInventory{}, err
	}
	for index := range repositories {
		expectedSource := expectedSources[repositories[index].Name]
		repositories[index].SourceVerification = ClassifySource(repositories[index], expectedSource)
	}
	return OwnerInventory{Owner: owner, Repositories: repositories}, nil
}

func (c *Client) GetRepository(ctx context.Context, owner string, repo string, expectedSource string) (Repository, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return Repository{}, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner, Repository: repo, Operation: "get_repository", Detail: "owner and repository name are required"}
	}

	var payload repositoryPayload
	_, err := c.getJSON(ctx, owner, repo, "get_repository", "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo), nil, &payload)
	if err != nil {
		return Repository{}, err
	}
	repository := payload.toRepository(time.Now().UTC())
	repository.SourceVerification = ClassifySource(repository, expectedSource)
	return repository, nil
}

func (c *Client) CreatePullMirror(ctx context.Context, req CreatePullMirrorRequest) (Repository, error) {
	owner := strings.TrimSpace(req.Owner)
	name := strings.TrimSpace(req.Name)
	cloneURL := strings.TrimSpace(req.CloneURL)
	if owner == "" || name == "" || cloneURL == "" {
		return Repository{}, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner, Repository: name, Operation: "create_pull_mirror", Detail: "owner, repository name, and clone URL are required"}
	}
	if redact.URLHasCredentials(cloneURL) {
		return Repository{}, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner, Repository: name, Operation: "create_pull_mirror", Detail: "clone URL must not contain credentials"}
	}
	if req.Private && strings.TrimSpace(req.GitHubToken) == "" {
		return Repository{}, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner, Repository: name, Operation: "create_pull_mirror", Detail: "private mirror creation requires GitHub credentials"}
	}
	if req.MirrorInterval <= 0 {
		return Repository{}, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner, Repository: name, Operation: "create_pull_mirror", Detail: "mirror interval must be positive"}
	}

	body := migrateRepoOptions{
		CloneAddr:      cloneURL,
		RepoName:       name,
		RepoOwner:      owner,
		Service:        "github",
		Mirror:         true,
		Private:        req.Private,
		MirrorInterval: req.MirrorInterval.String(),
		Wiki:           false,
		Issues:         false,
		PullRequests:   false,
		Releases:       false,
		Labels:         false,
		Milestones:     false,
		LFS:            false,
	}
	if req.Private {
		body.AuthToken = req.GitHubToken
	}

	var payload repositoryPayload
	_, err := c.postJSON(ctx, owner, name, "create_pull_mirror", "/repos/migrate", body, &payload, req.GitHubToken, cloneURL)
	if err != nil {
		return Repository{}, err
	}
	repository := payload.toRepository(time.Now().UTC())
	repository.SourceVerification = ClassifySource(repository, cloneURL)
	return repository, nil
}

func ClassifySource(repository Repository, expectedSource string) SourceVerification {
	if !repository.Mirror {
		return SourceNotApplicable
	}
	expectedSource = strings.TrimSpace(expectedSource)
	if repository.OriginalURL == "" || expectedSource == "" {
		return SourceUnverified
	}
	if sameSource(repository.OriginalURL, expectedSource) {
		return SourceVerified
	}
	return SourceMismatch
}

func (c *Client) listRepositories(ctx context.Context, owner Owner, operation string, endpoint string) ([]Repository, error) {
	var repositories []Repository
	for page := 1; ; page++ {
		query := map[string]string{
			"page":  strconv.Itoa(page),
			"limit": strconv.Itoa(pageLimit),
		}

		var payload []repositoryPayload
		info, err := c.getJSON(ctx, owner.Configured, "", operation, endpoint, query, &payload)
		if err != nil {
			return nil, err
		}
		observedAt := time.Now().UTC()
		for _, item := range payload {
			repositories = append(repositories, item.toRepository(observedAt))
		}
		if len(payload) == 0 {
			break
		}
		if total, ok := parseTotalCount(info.Header); ok && len(repositories) >= total {
			break
		}
	}
	return repositories, nil
}

func parseTotalCount(header http.Header) (int, bool) {
	value := strings.TrimSpace(header.Get("X-Total-Count"))
	if value == "" {
		return 0, false
	}
	total, err := strconv.Atoi(value)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}

func (c *Client) ownerFromOrganization(req OwnerValidationRequest, payload organizationPayload) Owner {
	name := payload.Name
	if name == "" {
		name = payload.UserName
	}
	if name == "" {
		name = req.Name
	}
	owner := Owner{
		Configured:    req.Name,
		Name:          name,
		ID:            payload.ID,
		Type:          OwnerTypeOrganization,
		TypeVerified:  true,
		Visibility:    payload.Visibility,
		ExpectedType:  req.ExpectedType,
		ObservedAtUTC: time.Now().UTC(),
	}
	if req.ExpectedType != "" && req.ExpectedType != OwnerTypeUnknown && req.ExpectedType != OwnerTypeOrganization {
		owner.TypeMismatch = true
	}
	return owner
}

func (c *Client) ownerFromUser(req OwnerValidationRequest, payload userPayload) (Owner, error) {
	name := payload.Login
	if name == "" {
		name = req.Name
	}
	owner := Owner{
		Configured:    req.Name,
		Name:          name,
		ID:            payload.ID,
		Type:          OwnerTypeUser,
		TypeVerified:  true,
		Visibility:    payload.Visibility,
		HTMLURL:       redact.SafeURL(payload.HTMLURL),
		ExpectedType:  req.ExpectedType,
		ObservedAtUTC: time.Now().UTC(),
	}
	if req.ExpectedType != "" && req.ExpectedType != OwnerTypeUnknown && req.ExpectedType != OwnerTypeUser {
		owner.TypeMismatch = true
		return owner, &Error{Code: "ERROR_GITEA_OWNER_TYPE", Owner: req.Name, Operation: "validate_owner", Detail: "Gitea owner is a user, expected organization"}
	}
	return owner, nil
}

type responseInfo struct {
	StatusCode int
	Header     http.Header
}

func (c *Client) getJSON(ctx context.Context, owner string, repo string, operation string, endpoint string, query map[string]string, target any, extraSecrets ...string) (responseInfo, error) {
	return c.doJSON(ctx, http.MethodGet, owner, repo, operation, endpoint, query, nil, target, extraSecrets...)
}

func (c *Client) postJSON(ctx context.Context, owner string, repo string, operation string, endpoint string, body any, target any, extraSecrets ...string) (responseInfo, error) {
	return c.doJSON(ctx, http.MethodPost, owner, repo, operation, endpoint, nil, body, target, extraSecrets...)
}

func (c *Client) doJSON(ctx context.Context, method string, owner string, repo string, operation string, endpoint string, query map[string]string, body any, target any, extraSecrets ...string) (responseInfo, error) {
	requestURL, err := c.requestURL(endpoint, query)
	if err != nil {
		return responseInfo{}, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner, Repository: repo, Operation: operation, Detail: c.redact(err.Error(), extraSecrets...)}
	}

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return responseInfo{}, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner, Repository: repo, Operation: operation, Detail: c.redact("encode request: "+err.Error(), extraSecrets...)}
		}
		reader = bytes.NewReader(data)
	}

	request, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return responseInfo{}, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner, Repository: repo, Operation: operation, Detail: c.redact(err.Error(), extraSecrets...)}
	}
	request.Header.Set("Authorization", "token "+c.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return responseInfo{}, &Error{Code: "ERROR_GITEA_ACCESS", Owner: owner, Repository: repo, Operation: operation, Detail: c.redact(err.Error(), extraSecrets...)}
	}
	defer response.Body.Close()

	info := responseInfo{StatusCode: response.StatusCode, Header: response.Header.Clone()}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return info, c.responseError(owner, repo, operation, response, extraSecrets...)
	}
	if target == nil {
		return info, nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return info, &Error{
			Code:       "ERROR_GITEA_ACCESS",
			Owner:      owner,
			Repository: repo,
			Operation:  operation,
			StatusCode: response.StatusCode,
			Detail:     c.redact("decode response: "+err.Error(), extraSecrets...),
		}
	}
	return info, nil
}

func (c *Client) responseError(owner string, repo string, operation string, response *http.Response, extraSecrets ...string) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	detail := strings.TrimSpace(c.redact(string(body), extraSecrets...))
	if detail == "" {
		detail = http.StatusText(response.StatusCode)
	}
	return &Error{
		Code:       "ERROR_GITEA_ACCESS",
		Owner:      owner,
		Repository: repo,
		Operation:  operation,
		StatusCode: response.StatusCode,
		Detail:     detail,
	}
}

func (c *Client) requestURL(endpoint string, query map[string]string) (string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	relative, err := url.Parse(strings.TrimPrefix(endpoint, "/"))
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

func (c *Client) redact(value string, extraSecrets ...string) string {
	secrets := append([]string{c.token}, extraSecrets...)
	value = redact.Secrets(value, secrets...)
	if redact.URLHasCredentials(value) {
		return redact.SafeURL(value)
	}
	return value
}

func normalizeBaseURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("Gitea base URL must be absolute")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	cleanPath := path.Clean("/" + parsed.Path)
	if cleanPath == "/" {
		cleanPath = ""
	}
	if !strings.HasSuffix(cleanPath, apiPrefix) {
		cleanPath = strings.TrimRight(cleanPath, "/") + apiPrefix
	}
	parsed.Path = strings.TrimRight(cleanPath, "/") + "/"
	return parsed.String(), nil
}

func isStatus(err error, status int) bool {
	apiErr, ok := err.(*Error)
	return ok && apiErr.StatusCode == status
}

func sameSource(left string, right string) bool {
	left = redact.SafeURL(strings.TrimSpace(left))
	right = redact.SafeURL(strings.TrimSpace(right))
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	leftNormalized, leftOK := normalizeSourceURL(left)
	rightNormalized, rightOK := normalizeSourceURL(right)
	return leftOK && rightOK && leftNormalized == rightNormalized
}

func normalizeSourceURL(value string) (string, bool) {
	parsed, err := url.Parse(redact.SafeURL(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	cleanPath := strings.TrimSuffix(path.Clean("/"+parsed.Path), ".git")
	if cleanPath == "/" || cleanPath == "." {
		return "", false
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host) + cleanPath, true
}

type organizationPayload struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	UserName   string `json:"username"`
	Visibility string `json:"visibility"`
}

type userPayload struct {
	ID         int64  `json:"id"`
	Login      string `json:"login"`
	HTMLURL    string `json:"html_url"`
	Visibility string `json:"visibility"`
}

type repositoryPayload struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	FullName       string `json:"full_name"`
	Private        bool   `json:"private"`
	Archived       bool   `json:"archived"`
	Fork           bool   `json:"fork"`
	HTMLURL        string `json:"html_url"`
	CloneURL       string `json:"clone_url"`
	SSHURL         string `json:"ssh_url"`
	Mirror         bool   `json:"mirror"`
	MirrorInterval string `json:"mirror_interval"`
	MirrorUpdated  string `json:"mirror_updated"`
	OriginalURL    string `json:"original_url"`
	Owner          struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (r repositoryPayload) toRepository(observedAt time.Time) Repository {
	owner := r.Owner.Login
	if owner == "" && strings.Contains(r.FullName, "/") {
		owner = strings.SplitN(r.FullName, "/", 2)[0]
	}
	mirrorUpdated := parseTime(r.MirrorUpdated)
	return Repository{
		ID:             r.ID,
		Owner:          owner,
		Name:           r.Name,
		FullName:       r.FullName,
		Private:        r.Private,
		Archived:       r.Archived,
		Fork:           r.Fork,
		HTMLURL:        redact.SafeURL(r.HTMLURL),
		CloneURL:       redact.SafeURL(r.CloneURL),
		SSHURL:         redact.SafeURL(r.SSHURL),
		Mirror:         r.Mirror,
		MirrorInterval: r.MirrorInterval,
		MirrorUpdated:  mirrorUpdated,
		OriginalURL:    redact.SafeURL(r.OriginalURL),
		ObservedAtUTC:  observedAt,
	}
}

func parseTime(value string) *time.Time {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

type migrateRepoOptions struct {
	AuthPassword   string `json:"auth_password,omitempty"`
	AuthToken      string `json:"auth_token,omitempty"`
	AuthUsername   string `json:"auth_username,omitempty"`
	CloneAddr      string `json:"clone_addr"`
	Issues         bool   `json:"issues"`
	Labels         bool   `json:"labels"`
	LFS            bool   `json:"lfs"`
	Milestones     bool   `json:"milestones"`
	Mirror         bool   `json:"mirror"`
	MirrorInterval string `json:"mirror_interval"`
	Private        bool   `json:"private"`
	PullRequests   bool   `json:"pull_requests"`
	Releases       bool   `json:"releases"`
	RepoName       string `json:"repo_name"`
	RepoOwner      string `json:"repo_owner"`
	Service        string `json:"service"`
	Wiki           bool   `json:"wiki"`
}
