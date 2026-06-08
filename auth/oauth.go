package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultAccessTokenTTL  = time.Hour
	defaultRefreshTokenTTL = 30 * 24 * time.Hour
	authCodeTTL            = 5 * time.Minute
)

// Server implements the OAuth endpoints MCP clients expect for HTTP transports.
// A caller-provided API-key validator authorizes the browser login, then issued
// access tokens are bound to this server's configured MCP resource URL.
type Server struct {
	apiKey          string
	publicBaseURL   string
	issuer          string
	issuerPath      string
	resourcePath    string
	metadataPath    string
	requiredScopes  []string
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
	isWhatsAppReady func() bool
	validateAPIKey  func(string) bool

	mu            sync.RWMutex
	clients       map[string]registeredClient
	authCodes     map[string]authorizationCode
	accessTokens  map[string]issuedToken
	refreshTokens map[string]issuedToken
}

type registeredClient struct {
	ID           string
	Secret       string
	Name         string
	RedirectURIs []string
	AuthMethod   string
	IssuedAt     time.Time
}

type authorizationCode struct {
	Code                string
	ClientID            string
	RedirectURI         string
	Scope               string
	Resource            string
	CodeChallenge       string
	CodeChallengeMethod string
	ExpiresAt           time.Time
}

type issuedToken struct {
	Token     string
	ClientID  string
	Scope     string
	Resource  string
	ExpiresAt time.Time
}

// Config controls OAuth and API-key behavior.
type Config struct {
	APIKey          string
	PublicBaseURL   string
	Issuer          string
	IssuerPath      string
	ResourcePath    string
	MetadataPath    string
	RequiredScopes  []string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	IsWhatsAppReady func() bool
	ValidateAPIKey  func(string) bool
}

// NewServer creates an in-memory OAuth authorization/resource server.
func NewServer(cfg Config) *Server {
	if cfg.AccessTokenTTL <= 0 {
		cfg.AccessTokenTTL = defaultAccessTokenTTL
	}
	if cfg.RefreshTokenTTL <= 0 {
		cfg.RefreshTokenTTL = defaultRefreshTokenTTL
	}
	if len(cfg.RequiredScopes) == 0 {
		cfg.RequiredScopes = []string{"whatsapp.read", "whatsapp.write"}
	}
	if cfg.IsWhatsAppReady == nil {
		cfg.IsWhatsAppReady = func() bool { return false }
	}
	if cfg.ResourcePath == "" {
		cfg.ResourcePath = "/mcp"
	}
	if cfg.IssuerPath == "" {
		cfg.IssuerPath = ""
	}
	if cfg.MetadataPath == "" {
		cfg.MetadataPath = "/.well-known/oauth-protected-resource"
	}
	if cfg.ValidateAPIKey == nil {
		cfg.ValidateAPIKey = func(value string) bool {
			return constantTimeEqual(value, cfg.APIKey)
		}
	}

	return &Server{
		apiKey:          cfg.APIKey,
		publicBaseURL:   strings.TrimRight(cfg.PublicBaseURL, "/"),
		issuer:          strings.TrimRight(cfg.Issuer, "/"),
		issuerPath:      cleanPath(cfg.IssuerPath),
		resourcePath:    cleanPath(cfg.ResourcePath),
		metadataPath:    cleanPath(cfg.MetadataPath),
		requiredScopes:  cfg.RequiredScopes,
		accessTokenTTL:  cfg.AccessTokenTTL,
		refreshTokenTTL: cfg.RefreshTokenTTL,
		isWhatsAppReady: cfg.IsWhatsAppReady,
		validateAPIKey:  cfg.ValidateAPIKey,
		clients:         make(map[string]registeredClient),
		authCodes:       make(map[string]authorizationCode),
		accessTokens:    make(map[string]issuedToken),
		refreshTokens:   make(map[string]issuedToken),
	}
}

// ValidateRequest returns true when the request has a valid API key or OAuth token.
func (s *Server) ValidateRequest(r *http.Request) bool {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		token = r.Header.Get("X-API-Key")
	}
	if token == "" {
		return false
	}
	if s.apiKey != "" && constantTimeEqual(token, s.apiKey) {
		return true
	}

	s.mu.RLock()
	issued, ok := s.accessTokens[token]
	s.mu.RUnlock()
	if !ok || time.Now().After(issued.ExpiresAt) {
		return false
	}

	resource := s.resourceURL(r)
	return resourceMatches(issued.Resource, resource)
}

// WriteUnauthorized sends the MCP OAuth discovery challenge.
func (s *Server) WriteUnauthorized(w http.ResponseWriter, r *http.Request) {
	challenge := fmt.Sprintf(
		`Bearer resource_metadata=%q, scope=%q`,
		s.protectedResourceMetadataURL(r),
		strings.Join(s.requiredScopes, " "),
	)
	w.Header().Set("WWW-Authenticate", challenge)
	writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "Missing or invalid access token")
}

// ProtectedResourceMetadata handles OAuth protected-resource metadata discovery.
func (s *Server) ProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	baseURL := s.baseURL(r)
	resp := map[string]any{
		"resource":                              s.resourceURL(r),
		"authorization_servers":                 []string{s.issuerURL(r)},
		"bearer_methods_supported":              []string{"header"},
		"scopes_supported":                      s.requiredScopes,
		"resource_documentation":                baseURL + "/health",
		"resource_signing_alg_values_supported": []string{},
	}
	writeJSON(w, http.StatusOK, resp)
}

// AuthorizationServerMetadata handles OAuth Authorization Server Metadata.
func (s *Server) AuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	issuer := s.issuerURL(r)
	resp := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"registration_endpoint":                 issuer + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
		"scopes_supported":                      s.requiredScopes,
		"resource_indicators_supported":         true,
		"client_id_metadata_document_supported": false,
	}
	writeJSON(w, http.StatusOK, resp)
}

// RegisterClient handles OAuth Dynamic Client Registration.
func (s *Server) RegisterClient(w http.ResponseWriter, r *http.Request) {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "Invalid registration JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, redirectURI := range req.RedirectURIs {
		if !validRedirectURI(redirectURI) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "Redirect URIs must use HTTPS or localhost HTTP")
			return
		}
	}

	authMethod := req.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}
	if authMethod != "none" && authMethod != "client_secret_basic" && authMethod != "client_secret_post" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "Unsupported token endpoint auth method")
		return
	}

	clientID := "mcp-" + randomString(24)
	client := registeredClient{
		ID:           clientID,
		Name:         req.ClientName,
		RedirectURIs: req.RedirectURIs,
		AuthMethod:   authMethod,
		IssuedAt:     time.Now(),
	}
	if authMethod != "none" {
		client.Secret = randomString(32)
	}

	s.mu.Lock()
	s.clients[clientID] = client
	s.mu.Unlock()

	resp := map[string]any{
		"client_id":                  client.ID,
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": client.AuthMethod,
		"client_id_issued_at":        client.IssuedAt.Unix(),
	}
	if client.Secret != "" {
		resp["client_secret"] = client.Secret
		resp["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, resp)
}

// Authorize handles the browser-based OAuth authorization-code login.
func (s *Server) Authorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderAuthorize(w, r, "")
	case http.MethodPost:
		s.completeAuthorize(w, r)
	default:
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "Method not allowed")
	}
}

func (s *Server) renderAuthorize(w http.ResponseWriter, r *http.Request, message string) {
	if errMsg := s.validateAuthorizeParams(r.FormValue); errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}

	data := struct {
		Action        string
		Message       string
		WhatsAppReady bool
		Params        map[string]string
	}{
		Action:        "/authorize",
		Message:       message,
		WhatsAppReady: s.isWhatsAppReady(),
		Params:        formParams(r),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = authorizeTemplate.Execute(w, data)
}

func (s *Server) completeAuthorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Invalid form body")
		return
	}
	if errMsg := s.validateAuthorizeParams(r.FormValue); errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	if !s.validateAPIKey(r.FormValue("api_key")) {
		s.renderAuthorize(w, r, "Invalid API key")
		return
	}

	code := "code-" + randomString(32)
	scope := r.FormValue("scope")
	if scope == "" {
		scope = strings.Join(s.requiredScopes, " ")
	}
	resource := r.FormValue("resource")
	if resource == "" {
		resource = s.resourceURL(r)
	}

	s.mu.Lock()
	s.authCodes[code] = authorizationCode{
		Code:                code,
		ClientID:            r.FormValue("client_id"),
		RedirectURI:         r.FormValue("redirect_uri"),
		Scope:               scope,
		Resource:            strings.TrimRight(resource, "/"),
		CodeChallenge:       r.FormValue("code_challenge"),
		CodeChallengeMethod: r.FormValue("code_challenge_method"),
		ExpiresAt:           time.Now().Add(authCodeTTL),
	}
	s.mu.Unlock()

	redirectURI, _ := url.Parse(r.FormValue("redirect_uri"))
	q := redirectURI.Query()
	q.Set("code", code)
	if state := r.FormValue("state"); state != "" {
		q.Set("state", state)
	}
	redirectURI.RawQuery = q.Encode()
	http.Redirect(w, r, redirectURI.String(), http.StatusFound)
}

// Token handles OAuth token exchange and refresh.
func (s *Server) Token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "Method not allowed")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "Invalid form body")
		return
	}

	switch r.FormValue("grant_type") {
	case "authorization_code":
		s.exchangeAuthorizationCode(w, r)
	case "refresh_token":
		s.exchangeRefreshToken(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "Unsupported grant type")
	}
}

func (s *Server) exchangeAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	clientID := r.FormValue("client_id")
	if !s.authenticateTokenClient(r, clientID) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "Invalid client authentication")
		return
	}

	codeValue := r.FormValue("code")
	s.mu.Lock()
	code, ok := s.authCodes[codeValue]
	if ok {
		delete(s.authCodes, codeValue)
	}
	s.mu.Unlock()

	if !ok || time.Now().After(code.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "Invalid or expired authorization code")
		return
	}
	if code.ClientID != clientID || code.RedirectURI != r.FormValue("redirect_uri") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "Authorization code was not issued to this client")
		return
	}
	if !verifyPKCE(code, r.FormValue("code_verifier")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}

	resource := r.FormValue("resource")
	if resource == "" {
		resource = code.Resource
	}
	if !resourceMatches(code.Resource, strings.TrimRight(resource, "/")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "Resource does not match authorization request")
		return
	}

	s.issueTokenResponse(w, code.ClientID, code.Scope, code.Resource)
}

func (s *Server) exchangeRefreshToken(w http.ResponseWriter, r *http.Request) {
	clientID := r.FormValue("client_id")
	if !s.authenticateTokenClient(r, clientID) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "Invalid client authentication")
		return
	}

	refreshTokenValue := r.FormValue("refresh_token")
	s.mu.RLock()
	refreshToken, ok := s.refreshTokens[refreshTokenValue]
	s.mu.RUnlock()
	if !ok || time.Now().After(refreshToken.ExpiresAt) || refreshToken.ClientID != clientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "Invalid or expired refresh token")
		return
	}

	s.issueTokenResponse(w, refreshToken.ClientID, refreshToken.Scope, refreshToken.Resource)
}

func (s *Server) issueTokenResponse(w http.ResponseWriter, clientID, scope, resource string) {
	access := "at-" + randomString(32)
	refresh := "rt-" + randomString(32)
	now := time.Now()

	s.mu.Lock()
	s.accessTokens[access] = issuedToken{
		Token:     access,
		ClientID:  clientID,
		Scope:     scope,
		Resource:  resource,
		ExpiresAt: now.Add(s.accessTokenTTL),
	}
	s.refreshTokens[refresh] = issuedToken{
		Token:     refresh,
		ClientID:  clientID,
		Scope:     scope,
		Resource:  resource,
		ExpiresAt: now.Add(s.refreshTokenTTL),
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int64(s.accessTokenTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         scope,
	})
}

func (s *Server) authenticateTokenClient(r *http.Request, clientID string) bool {
	if clientID == "" {
		if basicClientID, _, ok := r.BasicAuth(); ok {
			clientID = basicClientID
		}
	}
	s.mu.RLock()
	client, ok := s.clients[clientID]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	if client.Secret == "" {
		return true
	}
	if _, basicSecret, ok := r.BasicAuth(); ok {
		return constantTimeEqual(basicSecret, client.Secret)
	}
	return constantTimeEqual(r.FormValue("client_secret"), client.Secret)
}

func (s *Server) validateAuthorizeParams(get func(string) string) string {
	if get("response_type") != "code" {
		return "response_type=code is required"
	}
	clientID := get("client_id")
	redirectURI := get("redirect_uri")
	if clientID == "" || redirectURI == "" {
		return "client_id and redirect_uri are required"
	}

	s.mu.RLock()
	client, ok := s.clients[clientID]
	s.mu.RUnlock()
	if !ok {
		return "unknown client_id"
	}
	if !contains(client.RedirectURIs, redirectURI) {
		return "redirect_uri is not registered for this client"
	}
	if get("code_challenge") == "" {
		return "PKCE code_challenge is required"
	}
	method := get("code_challenge_method")
	if method == "" {
		method = "plain"
	}
	if method != "S256" && method != "plain" {
		return "unsupported code_challenge_method"
	}
	return ""
}

func (s *Server) baseURL(r *http.Request) string {
	if s.publicBaseURL != "" {
		return s.publicBaseURL
	}
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

func (s *Server) issuerURL(r *http.Request) string {
	if s.issuer != "" {
		return s.issuer
	}
	return s.baseURL(r) + s.issuerPath
}

func (s *Server) resourceURL(r *http.Request) string {
	return s.baseURL(r) + s.resourcePath
}

func (s *Server) protectedResourceMetadataURL(r *http.Request) string {
	return s.baseURL(r) + s.metadataPath
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

func cleanPath(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	return "/" + path
}

func constantTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func randomString(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func verifyPKCE(code authorizationCode, verifier string) bool {
	if code.CodeChallenge == "" {
		return false
	}
	if code.CodeChallengeMethod == "S256" {
		sum := sha256.Sum256([]byte(verifier))
		expected := base64.RawURLEncoding.EncodeToString(sum[:])
		return constantTimeEqual(expected, code.CodeChallenge)
	}
	return constantTimeEqual(verifier, code.CodeChallenge)
}

func resourceMatches(issued, requested string) bool {
	issued = strings.TrimRight(issued, "/")
	requested = strings.TrimRight(requested, "/")
	if issued == requested {
		return true
	}
	return strings.TrimSuffix(issued, "/mcp") == strings.TrimSuffix(requested, "/mcp")
}

func validRedirectURI(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func formParams(r *http.Request) map[string]string {
	keys := []string{
		"response_type",
		"client_id",
		"redirect_uri",
		"state",
		"scope",
		"code_challenge",
		"code_challenge_method",
		"resource",
	}
	params := make(map[string]string, len(keys))
	for _, key := range keys {
		params[key] = r.FormValue(key)
	}
	return params
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

var authorizeTemplate = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>WhatsApp MCP Login</title>
  <style>
    body { font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 0; min-height: 100vh; display: grid; place-items: center; background: #f6f7f9; color: #17202a; }
    main { width: min(440px, calc(100vw - 32px)); background: #fff; border: 1px solid #dfe4ea; border-radius: 8px; padding: 28px; box-shadow: 0 12px 28px rgba(15, 23, 42, .08); }
    h1 { font-size: 22px; margin: 0 0 10px; }
    p { line-height: 1.45; margin: 0 0 18px; color: #4b5563; }
    label { display: block; font-weight: 600; margin-bottom: 8px; }
    input[type=password] { width: 100%; box-sizing: border-box; font: inherit; padding: 10px 12px; border: 1px solid #cfd7e2; border-radius: 6px; }
    button { margin-top: 16px; width: 100%; font: inherit; font-weight: 700; color: #fff; background: #1f7a4d; border: 0; border-radius: 6px; padding: 11px 14px; cursor: pointer; }
    .status { margin: 14px 0; padding: 10px 12px; border-radius: 6px; background: #eef6f1; color: #245b3a; }
    .warn { background: #fff7ed; color: #9a3412; }
    .error { background: #fef2f2; color: #991b1b; }
  </style>
</head>
<body>
  <main>
    <h1>WhatsApp MCP Login</h1>
    <p>Enter this server's MCP API key to authorize your MCP client.</p>
    {{if .WhatsAppReady}}<div class="status">WhatsApp is linked.</div>{{else}}<div class="status warn">WhatsApp is not linked yet. Scan the QR code in the server logs, then continue.</div>{{end}}
    {{if .Message}}<div class="status error">{{.Message}}</div>{{end}}
    <form method="post" action="{{.Action}}">
      {{range $key, $value := .Params}}<input type="hidden" name="{{$key}}" value="{{$value}}">{{end}}
      <label for="api_key">API key</label>
      <input id="api_key" name="api_key" type="password" autocomplete="current-password" autofocus>
      <button type="submit">Authorize</button>
    </form>
  </main>
</body>
</html>`))
