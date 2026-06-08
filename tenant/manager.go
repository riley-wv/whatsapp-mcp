package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"whatsapp-mcp/auth"
	mcpserver "whatsapp-mcp/mcp"
	"whatsapp-mcp/paths"
	"whatsapp-mcp/storage"
	"whatsapp-mcp/webhook"
	"whatsapp-mcp/whatsapp"

	mcpgo "github.com/mark3labs/mcp-go/server"
	"github.com/skip2/go-qrcode"
)

const tenantsDir = "./data/tenants"

// Record is the persisted metadata for one isolated WhatsApp setup.
type Record struct {
	ID             string    `json:"id"`
	APIKeyHash     string    `json:"api_key_hash"`
	SetupTokenHash string    `json:"setup_token_hash"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreatedTenant contains the secrets shown once after setup creation.
type CreatedTenant struct {
	ID         string
	APIKey     string
	SetupToken string
}

// Instance is a live tenant runtime.
type Instance struct {
	Record           Record
	Paths            paths.InstancePaths
	DB               *sql.DB
	MessageStore     *storage.MessageStore
	MediaStore       *storage.MediaStore
	WebhookManager   *webhook.WebhookManager
	WhatsApp         *whatsapp.Client
	MCP              *mcpserver.MCPServer
	StreamableServer *mcpgo.StreamableHTTPServer
	OAuth            *auth.Server

	mu        sync.RWMutex
	qrEvent   string
	startedAt time.Time
}

// Manager owns tenant records and live tenant instances.
type Manager struct {
	registryPath string
	logLevel     string
	timezone     *time.Location
	logger       *log.Logger
	oauthClient  oauthClient

	mu        sync.RWMutex
	records   map[string]Record
	instances map[string]*Instance
	secrets   map[string]CreatedTenant
	oauth     *auth.Server

	pendingOAuth  map[string]pendingOAuthAuthorization
	authCodes     map[string]oauthGrant
	accessTokens  map[string]oauthGrant
	refreshTokens map[string]oauthGrant
}

type oauthClient struct {
	ID     string
	Secret string
}

type pendingOAuthAuthorization struct {
	ID                  string
	TenantID            string
	SetupToken          string
	ClientID            string
	RedirectURI         string
	State               string
	Scope               string
	Resource            string
	CodeChallenge       string
	CodeChallengeMethod string
	ExpiresAt           time.Time
}

type oauthGrant struct {
	TenantID            string
	ClientID            string
	RedirectURI         string
	Scope               string
	Resource            string
	CodeChallenge       string
	CodeChallengeMethod string
	ExpiresAt           time.Time
}

// NewManager loads the tenant registry.
func NewManager(logLevel string, timezone *time.Location, logger *log.Logger) (*Manager, error) {
	if logger == nil {
		logger = log.Default()
	}
	m := &Manager{
		registryPath: filepath.Join(tenantsDir, "registry.json"),
		logLevel:     logLevel,
		timezone:     timezone,
		logger:       logger,
		oauthClient: oauthClient{
			ID:     getenv("MCP_OAUTH_CLIENT_ID", "whatsapp-mcp"),
			Secret: getenv("MCP_OAUTH_CLIENT_SECRET", "change-this-shared-secret"),
		},
		records:       make(map[string]Record),
		instances:     make(map[string]*Instance),
		secrets:       make(map[string]CreatedTenant),
		pendingOAuth:  make(map[string]pendingOAuthAuthorization),
		authCodes:     make(map[string]oauthGrant),
		accessTokens:  make(map[string]oauthGrant),
		refreshTokens: make(map[string]oauthGrant),
	}
	m.oauth = auth.NewServer(auth.Config{
		ResourcePath:    "/mcp",
		IssuerPath:      "/oauth",
		MetadataPath:    "/.well-known/oauth-protected-resource",
		RequiredScopes:  []string{"whatsapp.read", "whatsapp.write"},
		IsWhatsAppReady: func() bool { return true },
		ResolveAPIKey:   m.TenantIDForAPIKey,
	})
	if err := os.MkdirAll(tenantsDir, 0o700); err != nil {
		return nil, err
	}
	if err := m.loadRegistry(); err != nil {
		return nil, err
	}
	return m, nil
}

// StartAll starts every persisted tenant.
func (m *Manager) StartAll() {
	m.mu.RLock()
	records := make([]Record, 0, len(m.records))
	for _, record := range m.records {
		records = append(records, record)
	}
	m.mu.RUnlock()

	for _, record := range records {
		if _, err := m.StartTenant(record.ID); err != nil {
			m.logger.Printf("Failed to start tenant %s: %v", record.ID, err)
		}
	}
}

// StopAll shuts down all live tenants.
func (m *Manager) StopAll() {
	m.mu.RLock()
	instances := make([]*Instance, 0, len(m.instances))
	for _, instance := range m.instances {
		instances = append(instances, instance)
	}
	m.mu.RUnlock()

	for _, instance := range instances {
		if instance.WebhookManager != nil {
			instance.WebhookManager.Stop()
		}
		if instance.WhatsApp != nil {
			instance.WhatsApp.Disconnect()
		}
		if instance.DB != nil {
			_ = instance.DB.Close()
		}
	}
}

// CreateTenant creates and starts a tenant, returning the API key once.
func (m *Manager) CreateTenant() (CreatedTenant, error) {
	created := CreatedTenant{
		ID:         randomID("wa"),
		APIKey:     "wmcp_" + randomToken(32),
		SetupToken: randomToken(32),
	}
	record := Record{
		ID:             created.ID,
		APIKeyHash:     hashSecret(created.APIKey),
		SetupTokenHash: hashSecret(created.SetupToken),
		CreatedAt:      time.Now(),
	}

	m.mu.Lock()
	m.records[record.ID] = record
	m.secrets[record.ID] = created
	err := m.saveRegistryLocked()
	m.mu.Unlock()
	if err != nil {
		return CreatedTenant{}, err
	}

	if _, err := m.StartTenant(record.ID); err != nil {
		return CreatedTenant{}, err
	}
	return created, nil
}

// StartTenant starts a tenant if it is not already running.
func (m *Manager) StartTenant(id string) (*Instance, error) {
	m.mu.RLock()
	if instance := m.instances[id]; instance != nil {
		m.mu.RUnlock()
		return instance, nil
	}
	record, ok := m.records[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("tenant not found")
	}

	instancePaths := paths.TenantInstancePaths(id)
	if err := paths.EnsureInstanceDirectories(instancePaths); err != nil {
		return nil, err
	}

	db, err := storage.InitDBAt(instancePaths.MessagesDBPath)
	if err != nil {
		return nil, err
	}
	store := storage.NewMessageStore(db)
	mediaStore := storage.NewMediaStore(db)

	webhookStore := storage.NewWebhookStore(db)
	webhookLogger := log.New(os.Stdout, "[WEBHOOK "+id+"] ", log.LstdFlags)
	webhookManager := webhook.NewWebhookManager(webhookStore, webhook.LoadConfig(), webhookLogger)
	webhookManager.Start()

	waClient, err := whatsapp.NewClientWithPaths(store, mediaStore, webhookManager, m.logLevel, instancePaths)
	if err != nil {
		_ = db.Close()
		webhookManager.Stop()
		return nil, err
	}

	instance := &Instance{
		Record:         record,
		Paths:          instancePaths,
		DB:             db,
		MessageStore:   store,
		MediaStore:     mediaStore,
		WebhookManager: webhookManager,
		WhatsApp:       waClient,
		startedAt:      time.Now(),
	}
	instance.MCP = mcpserver.NewMCPServerWithMediaDir(waClient, store, mediaStore, m.timezone, instancePaths.MediaDir)
	instance.StreamableServer = mcpgo.NewStreamableHTTPServer(
		instance.MCP.GetServer(),
		mcpgo.WithEndpointPath("/mcp"),
	)
	instance.OAuth = auth.NewServer(auth.Config{
		ResourcePath:    "/mcp/" + id,
		IssuerPath:      "/oauth/" + id,
		MetadataPath:    "/.well-known/oauth-protected-resource/mcp/" + id,
		RequiredScopes:  []string{"whatsapp.read", "whatsapp.write"},
		IsWhatsAppReady: waClient.IsLoggedIn,
		ValidateAPIKey:  func(apiKey string) bool { return constantTimeEqual(hashSecret(apiKey), record.APIKeyHash) },
	})

	m.mu.Lock()
	m.instances[id] = instance
	m.mu.Unlock()

	if waClient.IsLoggedIn() {
		if err := waClient.Connect(); err != nil {
			m.logger.Printf("Failed to connect tenant %s to WhatsApp: %v", id, err)
		}
	} else {
		go m.startWhatsAppSetup(instance)
	}

	return instance, nil
}

// HandleSetup serves /setup and /setup/{tenant_id}.
func (m *Manager) HandleSetup(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/setup"), "/")
	if trimmed == "" {
		if r.Method == http.MethodPost {
			m.createSetup(w, r)
			return
		}
		m.renderCreateSetup(w, r)
		return
	}

	parts := strings.Split(trimmed, "/")
	id := parts[0]
	if len(parts) == 2 && parts[1] == "qr.png" {
		m.serveQRCode(w, r, id)
		return
	}
	m.renderTenantSetup(w, r, id, "")
}

// HandleMCP serves /mcp as a single public URL and /mcp/{tenant_id} as a
// backward-compatible explicit tenant URL.
func (m *Manager) HandleMCP(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/mcp"), "/")
	if trimmed == "" {
		id, ok := m.TenantIDForRequest(r)
		if !ok {
			m.oauth.WriteUnauthorized(w, r)
			return
		}
		instance, ok := m.Instance(id)
		if !ok {
			http.Error(w, "tenant not found", http.StatusNotFound)
			return
		}
		r.URL.Path = "/mcp"
		instance.StreamableServer.ServeHTTP(w, r)
		return
	}

	parts := strings.SplitN(trimmed, "/", 2)
	id := parts[0]
	instance, ok := m.Instance(id)
	if !ok {
		http.Error(w, "tenant not found", http.StatusNotFound)
		return
	}
	if !m.ValidateAPIKey(r, id) && !instance.OAuth.ValidateRequest(r) {
		instance.OAuth.WriteUnauthorized(w, r)
		return
	}

	if len(parts) == 2 {
		r.URL.Path = "/mcp/" + parts[1]
	} else {
		r.URL.Path = "/mcp"
	}
	instance.StreamableServer.ServeHTTP(w, r)
}

// HandleOAuth serves root and tenant-specific OAuth discovery, registration,
// authorize, and token endpoints.
func (m *Manager) HandleOAuth(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/oauth"), "/")
	if trimmed == "" {
		http.NotFound(w, r)
		return
	}

	if !strings.HasPrefix(trimmed, "wa_") {
		m.handleRootOAuth(trimmed, w, r)
		return
	}

	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	instance, ok := m.Instance(parts[0])
	if !ok {
		http.NotFound(w, r)
		return
	}

	m.handleOAuthServer(instance.OAuth, parts[1], w, r)
}

func (m *Manager) handleOAuthServer(oauthServer *auth.Server, path string, w http.ResponseWriter, r *http.Request) {
	switch "/" + strings.Trim(path, "/") {
	case "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration":
		oauthServer.AuthorizationServerMetadata(w, r)
	case "/register":
		oauthServer.RegisterClient(w, r)
	case "/authorize":
		oauthServer.Authorize(w, r)
	case "/token":
		oauthServer.Token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *Manager) handleRootOAuth(path string, w http.ResponseWriter, r *http.Request) {
	switch "/" + strings.Trim(path, "/") {
	case "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration":
		m.rootOAuthMetadata(w, r)
	case "/register":
		m.rootOAuthRegister(w, r)
	case "/configure", "/oauth/configure":
		m.rootOAuthConfigure(w, r)
	case "/authorize":
		m.rootOAuthAuthorize(w, r)
	case "/token":
		m.rootOAuthToken(w, r)
	default:
		http.NotFound(w, r)
	}
}

// HandleWellKnownOAuth serves issuer-derived OAuth/OIDC discovery URLs, such as
// /.well-known/openid-configuration/oauth for an issuer of https://host/oauth.
func (m *Manager) HandleWellKnownOAuth(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	switch path {
	case ".well-known/openid-configuration",
		".well-known/openid-configuration/oauth",
		".well-known/oauth-authorization-server",
		".well-known/oauth-authorization-server/oauth":
		m.rootOAuthMetadata(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *Manager) rootOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	issuer := baseURL(r) + "/oauth"
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"registration_endpoint":                 issuer + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"scopes_supported":                      []string{"whatsapp.read", "whatsapp.write"},
		"resource_indicators_supported":         true,
	})
}

func (m *Manager) rootOAuthConfigure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "Method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client_id":                  m.oauthClient.ID,
		"client_secret":              m.oauthClient.Secret,
		"client_secret_expires_at":   0,
		"token_endpoint_auth_method": "client_secret_basic",
		"scope":                      "whatsapp.read whatsapp.write",
		"authorization_endpoint":     baseURL(r) + "/oauth/authorize",
		"token_endpoint":             baseURL(r) + "/oauth/token",
	})
}

func (m *Manager) rootOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "Method not allowed")
		return
	}
	var req struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		Scope                   string   `json:"scope"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ClientName == "" {
		req.ClientName = "WhatsApp MCP"
	}
	if len(req.GrantTypes) == 0 {
		req.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	if len(req.ResponseTypes) == 0 {
		req.ResponseTypes = []string{"code"}
	}
	if req.Scope == "" {
		req.Scope = "whatsapp.read whatsapp.write"
	}
	// Poke may register as a public client, but this server intentionally uses
	// one shared confidential OAuth client for every integration.
	req.TokenEndpointAuthMethod = "client_secret_basic"

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  m.oauthClient.ID,
		"client_secret":              m.oauthClient.Secret,
		"client_secret_expires_at":   0,
		"client_name":                req.ClientName,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                req.GrantTypes,
		"response_types":             req.ResponseTypes,
		"token_endpoint_auth_method": req.TokenEndpointAuthMethod,
		"scope":                      req.Scope,
	})
}

func (m *Manager) rootOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		m.rootOAuthAuthorizeGet(w, r)
	case http.MethodPost:
		m.rootOAuthAuthorizePost(w, r)
	default:
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "Method not allowed")
	}
}

func (m *Manager) rootOAuthAuthorizeGet(w http.ResponseWriter, r *http.Request) {
	if sessionID := r.URL.Query().Get("oauth_session"); sessionID != "" {
		m.renderOAuthSession(w, r, sessionID, "")
		return
	}

	if errMsg := m.validateRootAuthorizeRequest(r); errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}

	created, err := m.CreateTenant()
	if err != nil {
		http.Error(w, "failed to create WhatsApp setup", http.StatusInternalServerError)
		return
	}

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "whatsapp.read whatsapp.write"
	}
	resource := r.URL.Query().Get("resource")
	if resource == "" {
		resource = baseURL(r) + "/mcp"
	}
	codeChallengeMethod := r.URL.Query().Get("code_challenge_method")
	if codeChallengeMethod == "" {
		codeChallengeMethod = "plain"
	}

	sessionID := "oas_" + randomToken(24)
	pending := pendingOAuthAuthorization{
		ID:                  sessionID,
		TenantID:            created.ID,
		SetupToken:          created.SetupToken,
		ClientID:            r.URL.Query().Get("client_id"),
		RedirectURI:         r.URL.Query().Get("redirect_uri"),
		State:               r.URL.Query().Get("state"),
		Scope:               scope,
		Resource:            strings.TrimRight(resource, "/"),
		CodeChallenge:       r.URL.Query().Get("code_challenge"),
		CodeChallengeMethod: codeChallengeMethod,
		ExpiresAt:           time.Now().Add(20 * time.Minute),
	}

	m.mu.Lock()
	m.pendingOAuth[sessionID] = pending
	m.mu.Unlock()

	http.Redirect(w, r, "/oauth/authorize?oauth_session="+url.QueryEscape(sessionID), http.StatusFound)
}

func (m *Manager) rootOAuthAuthorizePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Invalid form body")
		return
	}
	sessionID := r.FormValue("oauth_session")
	pending, ok := m.getPendingOAuth(sessionID)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Authorization session expired")
		return
	}

	instance, ok := m.Instance(pending.TenantID)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "WhatsApp setup not found")
		return
	}
	if !instance.WhatsApp.IsLoggedIn() {
		m.renderOAuthSession(w, r, sessionID, "Scan the QR code with WhatsApp before continuing.")
		return
	}

	code := "code-" + randomToken(32)
	m.mu.Lock()
	delete(m.pendingOAuth, sessionID)
	m.authCodes[code] = oauthGrant{
		TenantID:            pending.TenantID,
		ClientID:            pending.ClientID,
		RedirectURI:         pending.RedirectURI,
		Scope:               pending.Scope,
		Resource:            pending.Resource,
		CodeChallenge:       pending.CodeChallenge,
		CodeChallengeMethod: pending.CodeChallengeMethod,
		ExpiresAt:           time.Now().Add(5 * time.Minute),
	}
	m.mu.Unlock()

	redirectURI, _ := url.Parse(pending.RedirectURI)
	q := redirectURI.Query()
	q.Set("code", code)
	if pending.State != "" {
		q.Set("state", pending.State)
	}
	redirectURI.RawQuery = q.Encode()
	http.Redirect(w, r, redirectURI.String(), http.StatusFound)
}

func (m *Manager) rootOAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "Method not allowed")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Invalid form body")
		return
	}
	clientID := r.FormValue("client_id")
	if clientID == "" {
		if basicID, _, ok := r.BasicAuth(); ok {
			clientID = basicID
		}
	}
	if !m.authenticateRootOAuthClient(r, clientID) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "Invalid client credentials")
		return
	}

	switch r.FormValue("grant_type") {
	case "authorization_code":
		m.exchangeRootAuthorizationCode(w, r, clientID)
	case "refresh_token":
		m.exchangeRootRefreshToken(w, r, clientID)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "Unsupported grant type")
	}
}

func (m *Manager) exchangeRootAuthorizationCode(w http.ResponseWriter, r *http.Request, clientID string) {
	codeValue := r.FormValue("code")
	m.mu.Lock()
	grant, ok := m.authCodes[codeValue]
	if ok {
		delete(m.authCodes, codeValue)
	}
	m.mu.Unlock()

	if !ok || time.Now().After(grant.ExpiresAt) || grant.ClientID != clientID || grant.RedirectURI != r.FormValue("redirect_uri") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "Invalid or expired authorization code")
		return
	}
	if grant.CodeChallenge != "" && !verifyPKCE(grant.CodeChallenge, grant.CodeChallengeMethod, r.FormValue("code_verifier")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	m.issueRootTokenResponse(w, grant)
}

func (m *Manager) exchangeRootRefreshToken(w http.ResponseWriter, r *http.Request, clientID string) {
	refreshValue := r.FormValue("refresh_token")
	m.mu.RLock()
	grant, ok := m.refreshTokens[refreshValue]
	m.mu.RUnlock()
	if !ok || time.Now().After(grant.ExpiresAt) || grant.ClientID != clientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "Invalid or expired refresh token")
		return
	}
	m.issueRootTokenResponse(w, grant)
}

func (m *Manager) issueRootTokenResponse(w http.ResponseWriter, grant oauthGrant) {
	access := "at-" + randomToken(32)
	refresh := "rt-" + randomToken(32)
	now := time.Now()

	accessGrant := grant
	accessGrant.ExpiresAt = now.Add(time.Hour)
	refreshGrant := grant
	refreshGrant.ExpiresAt = now.Add(30 * 24 * time.Hour)

	m.mu.Lock()
	m.accessTokens[access] = accessGrant
	m.refreshTokens[refresh] = refreshGrant
	m.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int64(time.Hour.Seconds()),
		"refresh_token": refresh,
		"scope":         grant.Scope,
		"resource":      grant.Resource,
	})
}

func (m *Manager) authenticateRootOAuthClient(r *http.Request, clientID string) bool {
	if clientID != m.oauthClient.ID {
		return false
	}
	if _, secret, ok := r.BasicAuth(); ok {
		return constantTimeEqual(secret, m.oauthClient.Secret)
	}
	return constantTimeEqual(r.FormValue("client_secret"), m.oauthClient.Secret)
}

func (m *Manager) validateRootAuthorizeRequest(r *http.Request) string {
	q := r.URL.Query()
	if q.Get("response_type") != "code" {
		return "response_type=code is required"
	}
	if q.Get("client_id") != m.oauthClient.ID {
		return "invalid client_id"
	}
	if q.Get("redirect_uri") == "" {
		return "redirect_uri is required"
	}
	if _, err := url.Parse(q.Get("redirect_uri")); err != nil {
		return "invalid redirect_uri"
	}
	method := q.Get("code_challenge_method")
	if method == "" {
		method = "plain"
	}
	if q.Get("code_challenge") != "" && method != "S256" && method != "plain" {
		return "unsupported code_challenge_method"
	}
	return ""
}

func (m *Manager) renderOAuthSession(w http.ResponseWriter, r *http.Request, sessionID, message string) {
	pending, ok := m.getPendingOAuth(sessionID)
	if !ok {
		http.Error(w, "authorization session expired", http.StatusBadRequest)
		return
	}
	instance, ok := m.Instance(pending.TenantID)
	if !ok {
		http.Error(w, "WhatsApp setup not found", http.StatusBadRequest)
		return
	}
	instance.mu.RLock()
	qrEvent := instance.qrEvent
	instance.mu.RUnlock()

	data := map[string]any{
		"OAuthSession":  sessionID,
		"SetupToken":    pending.SetupToken,
		"TenantID":      pending.TenantID,
		"WhatsAppReady": instance.WhatsApp.IsLoggedIn(),
		"QREvent":       qrEvent,
		"HasQRCode":     fileExists(instance.Paths.QRCodePath),
		"QRURL":         "/setup/" + pending.TenantID + "/qr.png?setup_token=" + urlQueryEscape(pending.SetupToken),
		"Message":       message,
	}
	renderHTML(w, oauthAuthorizeTemplate, data)
}

func (m *Manager) getPendingOAuth(sessionID string) (pendingOAuthAuthorization, bool) {
	m.mu.RLock()
	pending, ok := m.pendingOAuth[sessionID]
	m.mu.RUnlock()
	if !ok || time.Now().After(pending.ExpiresAt) {
		return pendingOAuthAuthorization{}, false
	}
	return pending, true
}

// HandleProtectedResourceMetadata serves tenant-specific MCP OAuth protected-resource metadata.
func (m *Manager) HandleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/.well-known/oauth-protected-resource"), "/")
	if trimmed == "" || trimmed == "oauth" {
		m.oauth.ProtectedResourceMetadata(w, r)
		return
	}

	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] != "mcp" {
		http.NotFound(w, r)
		return
	}
	instance, ok := m.Instance(parts[1])
	if !ok {
		http.NotFound(w, r)
		return
	}
	instance.OAuth.ProtectedResourceMetadata(w, r)
}

// TenantIDForRequest returns the tenant ID bound to a direct API key or root OAuth token.
func (m *Manager) TenantIDForRequest(r *http.Request) (string, bool) {
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		token = r.Header.Get("X-API-Key")
	}
	if token == "" {
		return "", false
	}
	if id, ok := m.TenantIDForAPIKey(token); ok {
		return id, true
	}
	if id, ok := m.TenantIDForOAuthToken(token); ok {
		return id, true
	}
	return m.oauth.SubjectForRequest(r)
}

// TenantIDForOAuthToken finds the tenant mapped to a root OAuth access token.
func (m *Manager) TenantIDForOAuthToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	m.mu.RLock()
	grant, ok := m.accessTokens[token]
	m.mu.RUnlock()
	if !ok || time.Now().After(grant.ExpiresAt) {
		return "", false
	}
	return grant.TenantID, true
}

// TenantIDForAPIKey finds the tenant whose API-key hash matches apiKey.
func (m *Manager) TenantIDForAPIKey(apiKey string) (string, bool) {
	if apiKey == "" {
		return "", false
	}
	hash := hashSecret(apiKey)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, record := range m.records {
		if constantTimeEqual(hash, record.APIKeyHash) {
			return record.ID, true
		}
	}
	return "", false
}

// Instance returns a live tenant instance.
func (m *Manager) Instance(id string) (*Instance, bool) {
	m.mu.RLock()
	instance, ok := m.instances[id]
	m.mu.RUnlock()
	return instance, ok
}

// ValidateAPIKey validates Authorization: Bearer or X-API-Key for a tenant.
func (m *Manager) ValidateAPIKey(r *http.Request, id string) bool {
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		token = r.Header.Get("X-API-Key")
	}
	if token == "" {
		return false
	}

	m.mu.RLock()
	record, ok := m.records[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	return constantTimeEqual(hashSecret(token), record.APIKeyHash)
}

func (m *Manager) createSetup(w http.ResponseWriter, r *http.Request) {
	created, err := m.CreateTenant()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/setup/"+created.ID+"?setup_token="+urlQueryEscape(created.SetupToken), http.StatusFound)
}

func (m *Manager) renderCreateSetup(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"BaseURL": baseURL(r),
	}
	renderHTML(w, createSetupTemplate, data)
}

func (m *Manager) renderTenantSetup(w http.ResponseWriter, r *http.Request, id, message string) {
	if !m.ValidateSetupAccess(r, id) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	instance, ok := m.Instance(id)
	if !ok {
		http.Error(w, "tenant not running", http.StatusNotFound)
		return
	}
	m.mu.RLock()
	apiKey := ""
	if secret := m.secrets[id]; secret.ID == id {
		apiKey = secret.APIKey
	}
	m.mu.RUnlock()
	instance.mu.RLock()
	qrEvent := instance.qrEvent
	instance.mu.RUnlock()

	data := map[string]any{
		"ID":             id,
		"APIKey":         apiKey,
		"SetupToken":     r.URL.Query().Get("setup_token"),
		"MCPURL":         baseURL(r) + "/mcp",
		"WhatsAppReady":  instance.WhatsApp.IsLoggedIn(),
		"QREvent":        qrEvent,
		"HasQRCode":      fileExists(instance.Paths.QRCodePath),
		"Message":        message,
		"CreatedAt":      instance.Record.CreatedAt.Format(time.RFC3339),
		"APIKeyWasShown": apiKey != "",
	}
	renderHTML(w, tenantSetupTemplate, data)
}

func (m *Manager) serveQRCode(w http.ResponseWriter, r *http.Request, id string) {
	if !m.ValidateSetupAccess(r, id) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	instance, ok := m.Instance(id)
	if !ok || !fileExists(instance.Paths.QRCodePath) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, instance.Paths.QRCodePath)
}

// ValidateSetupAccess validates the setup token or tenant API key.
func (m *Manager) ValidateSetupAccess(r *http.Request, id string) bool {
	setupToken := r.URL.Query().Get("setup_token")
	m.mu.RLock()
	record, ok := m.records[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	if setupToken != "" && constantTimeEqual(hashSecret(setupToken), record.SetupTokenHash) {
		return true
	}
	return m.ValidateAPIKey(r, id)
}

func (m *Manager) startWhatsAppSetup(instance *Instance) {
	m.logger.Printf("Tenant %s is not linked. Open the setup URL returned when the tenant was created to scan the QR code.", instance.Record.ID)
	qrChan, err := instance.WhatsApp.GetQRChannel(context.Background())
	if err != nil {
		m.logger.Printf("Failed to start WhatsApp setup for tenant %s: %v", instance.Record.ID, err)
		return
	}
	for evt := range qrChan {
		instance.mu.Lock()
		instance.qrEvent = evt.Event
		instance.mu.Unlock()
		if evt.Event == "code" {
			if err := qrcode.WriteFile(evt.Code, qrcode.Low, 256, instance.Paths.QRCodePath); err != nil {
				m.logger.Printf("Failed to write QR code for tenant %s: %v", instance.Record.ID, err)
			}
		}
	}
	if instance.WhatsApp.IsLoggedIn() {
		m.logger.Printf("Tenant %s linked to WhatsApp", instance.Record.ID)
	}
}

func (m *Manager) loadRegistry() error {
	data, err := os.ReadFile(m.registryPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	for _, record := range records {
		m.records[record.ID] = record
	}
	return nil
}

func (m *Manager) saveRegistryLocked() error {
	records := make([]Record, 0, len(m.records))
	for _, record := range m.records {
		records = append(records, record)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.registryPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.registryPath)
}

func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomID(prefix string) string {
	return prefix + "_" + randomToken(12)
}

func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func baseURL(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return strings.TrimRight(proto+"://"+host, "/")
}

func urlQueryEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func renderHTML(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = tmpl.Execute(w, data)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": description,
	})
}

func verifyPKCE(challenge, method, verifier string) bool {
	if challenge == "" {
		return false
	}
	if method == "S256" {
		sum := sha256.Sum256([]byte(verifier))
		expected := base64.RawURLEncoding.EncodeToString(sum[:])
		return constantTimeEqual(expected, challenge)
	}
	return constantTimeEqual(verifier, challenge)
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

var createSetupTemplate = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>WhatsApp MCP Setup</title>
  <style>
    body { font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 0; min-height: 100vh; display: grid; place-items: center; background: #f6f7f9; color: #17202a; }
    main { width: min(520px, calc(100vw - 32px)); background: #fff; border: 1px solid #dfe4ea; border-radius: 8px; padding: 28px; box-shadow: 0 12px 28px rgba(15, 23, 42, .08); }
    h1 { font-size: 24px; margin: 0 0 10px; }
    p { color: #4b5563; line-height: 1.45; }
    button { font: inherit; font-weight: 700; color: #fff; background: #1f7a4d; border: 0; border-radius: 6px; padding: 11px 16px; cursor: pointer; }
  </style>
</head>
<body>
  <main>
    <h1>Set Up WhatsApp MCP</h1>
    <p>Create an isolated WhatsApp connection. The next page shows a WhatsApp QR code and a new API key for this connection.</p>
    <form method="post" action="/setup"><button type="submit">Create WhatsApp Setup</button></form>
  </main>
</body>
</html>`))

var oauthAuthorizeTemplate = template.Must(template.New("oauth-authorize").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  {{if not .WhatsAppReady}}<meta http-equiv="refresh" content="8">{{end}}
  <title>Connect WhatsApp</title>
  <style>
    body { font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 0; min-height: 100vh; display: grid; place-items: center; background: #f6f7f9; color: #17202a; }
    main { width: min(520px, calc(100vw - 32px)); background: #fff; border: 1px solid #dfe4ea; border-radius: 8px; padding: 28px; box-shadow: 0 12px 28px rgba(15, 23, 42, .08); }
    h1 { font-size: 24px; margin: 0 0 10px; }
    p { color: #4b5563; line-height: 1.45; }
    img { width: 256px; height: 256px; image-rendering: crisp-edges; border: 1px solid #dfe4ea; border-radius: 6px; }
    button { font: inherit; font-weight: 700; color: #fff; background: #1f7a4d; border: 0; border-radius: 6px; padding: 11px 16px; cursor: pointer; }
    .status { margin: 14px 0; padding: 10px 12px; border-radius: 6px; background: #eef6f1; color: #245b3a; }
    .warn { background: #fff7ed; color: #9a3412; }
    .error { background: #fef2f2; color: #991b1b; }
  </style>
</head>
<body>
  <main>
    <h1>Connect WhatsApp</h1>
    {{if .WhatsAppReady}}
      <div class="status">WhatsApp is connected and verified.</div>
      <p>Continue to finish authorizing your MCP client.</p>
      <form method="post" action="/oauth/authorize">
        <input type="hidden" name="oauth_session" value="{{.OAuthSession}}">
        <button type="submit">Continue</button>
      </form>
    {{else}}
      <div class="status warn">Waiting for WhatsApp connection.</div>
      {{if .Message}}<div class="status error">{{.Message}}</div>{{end}}
      {{if eq .QREvent "err-client-outdated"}}
        <p>WhatsApp rejected this client as outdated. Restart the server so it can refresh the WhatsApp Web version, or set WHATSAPP_WEB_VERSION manually.</p>
      {{else if .HasQRCode}}
        <img alt="WhatsApp setup QR code" src="{{.QRURL}}">
        <p>Open WhatsApp on your phone, go to Linked devices, and scan this QR code. This page refreshes automatically.</p>
      {{else}}
        <p>QR code is being generated. This page refreshes automatically.</p>
      {{end}}
    {{end}}
  </main>
</body>
</html>`))

var tenantSetupTemplate = template.Must(template.New("tenant").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="10">
  <title>WhatsApp MCP Setup</title>
  <style>
    body { font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 0; min-height: 100vh; background: #f6f7f9; color: #17202a; }
    main { width: min(760px, calc(100vw - 32px)); margin: 32px auto; background: #fff; border: 1px solid #dfe4ea; border-radius: 8px; padding: 28px; box-shadow: 0 12px 28px rgba(15, 23, 42, .08); }
    h1 { font-size: 24px; margin: 0 0 10px; }
    h2 { font-size: 15px; margin: 24px 0 8px; }
    p { color: #4b5563; line-height: 1.45; }
    code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
    pre { overflow-x: auto; background: #f3f4f6; border-radius: 6px; padding: 12px; }
    img { width: 256px; height: 256px; image-rendering: crisp-edges; border: 1px solid #dfe4ea; border-radius: 6px; }
    .status { display: inline-block; padding: 6px 10px; border-radius: 999px; background: #eef6f1; color: #245b3a; font-weight: 700; }
    .warn { background: #fff7ed; color: #9a3412; }
  </style>
</head>
<body>
  <main>
    <h1>WhatsApp MCP Setup</h1>
    {{if .WhatsAppReady}}<span class="status">WhatsApp linked</span>{{else}}<span class="status warn">Waiting for WhatsApp link</span>{{end}}
    <h2>Tenant ID</h2>
    <pre>{{.ID}}</pre>
    <h2>MCP URL</h2>
    <pre>{{.MCPURL}}</pre>
    <h2>API Key</h2>
    {{if .APIKey}}<pre>{{.APIKey}}</pre><p>This API key is shown during setup. Store it securely; after a restart, this page can validate it but cannot recover it.</p>{{else}}<p>The API key was already shown. Use the key you saved for this tenant.</p>{{end}}
    {{if not .WhatsAppReady}}
      <h2>WhatsApp QR</h2>
      {{if eq .QREvent "err-client-outdated"}}<p>WhatsApp rejected this client as outdated. Restart the server so it can refresh the WhatsApp Web version, or set WHATSAPP_WEB_VERSION manually.</p>{{else if .HasQRCode}}<img alt="WhatsApp setup QR code" src="/setup/{{.ID}}/qr.png?setup_token={{.SetupToken}}">{{else}}<p>QR code is being generated. This page refreshes automatically.</p>{{end}}
      <p>Open WhatsApp on your phone, go to Linked devices, and scan this QR code.</p>
    {{end}}
  </main>
</body>
</html>`))
