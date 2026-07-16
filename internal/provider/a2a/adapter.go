package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/secret"
)

var (
	canonicalProviderName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	scopedProviderID      = regexp.MustCompile(`^a2a/ws-[0-9a-f]{16}/[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

var errUnsupportedRequiredExtension = errors.New("unsupported required A2A extension")

type Adapter struct {
	client               *http.Client
	mu                   sync.Mutex
	cache                map[string]resolvedProvider
	active               map[string]*activeTask
	activeInvocations    int
	maxActiveInvocations int
	pollInterval         time.Duration
	maxPollInterval      time.Duration
	taskTimeout          time.Duration
	resolver             secret.Resolver
}

type activeTask struct {
	providerID string
	taskID     string
	tenant     string
	bearer     bool
	cancel     context.CancelFunc
	updates    chan json.RawMessage
	canceling  bool
	terminalMu sync.Mutex
	owner      terminalOwner
}

type terminalOwner uint8

const (
	terminalUnclaimed terminalOwner = iota
	terminalPoll
	terminalCancel
)

func New() *Adapter {
	return NewWithResolver(nil)
}

func NewWithResolver(resolver secret.Resolver) *Adapter {
	return &Adapter{
		client:               &http.Client{Timeout: 30 * time.Second, CheckRedirect: safeRedirect},
		cache:                map[string]resolvedProvider{},
		active:               map[string]*activeTask{},
		maxActiveInvocations: 64,
		pollInterval:         250 * time.Millisecond,
		maxPollInterval:      5 * time.Second,
		taskTimeout:          30 * time.Minute,
		resolver:             resolver,
	}
}

func (a *Adapter) acquireInvocation() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeInvocations >= a.maxActiveInvocations {
		return false
	}
	a.activeInvocations++
	return true
}

func (a *Adapter) releaseInvocation() {
	a.mu.Lock()
	a.activeInvocations--
	a.mu.Unlock()
}

func safeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 3 {
		return errors.New("too many A2A redirects")
	}
	if len(via) == 0 {
		return nil
	}
	first := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, first.Scheme) || !strings.EqualFold(req.URL.Host, first.Host) {
		return errors.New("A2A redirect changed origin")
	}
	if strings.EqualFold(first.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return errors.New("A2A redirect downgraded HTTPS")
	}
	return nil
}

func validateProvider(p provider.Provider) error {
	if p.Protocol != "a2a" || !scopedProviderID.MatchString(p.ID) || !strings.HasSuffix(p.ID, "/"+p.Name) || !canonicalProviderName.MatchString(p.Name) {
		return errors.New("A2A provider name must be a canonical non-empty slug")
	}
	return nil
}

func discoveryURL(endpoint string) (*url.URL, error) {
	if len(endpoint) == 0 || len(endpoint) > maxStringBytes {
		return nil, errors.New("invalid A2A provider endpoint")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid A2A provider endpoint")
	}
	if err := validateTransportURL(u); err != nil {
		return nil, err
	}
	if path.Base(u.Path) == "agent-card.json" {
		return u, nil
	}
	u.Path = "/.well-known/agent-card.json"
	u.RawPath = ""
	u.RawQuery = ""
	return u, nil
}

func loopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateTransportURL(u *url.URL) error {
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return errors.New("A2A cleartext HTTP requires a loopback host")
	}
	return nil
}

func canonicalOrigin(u *url.URL) string {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(host, port)
}

type securityOptions struct{ public, bearer bool }

func validateSecurityRequirements(card agentCard, requirements []agentSecurityRequirement) (securityOptions, error) {
	if len(requirements) == 0 {
		return securityOptions{public: true}, nil
	}
	if len(requirements) > maxListItems || len(card.SecuritySchemes) > maxListItems {
		return securityOptions{}, errors.New("invalid A2A security requirements")
	}
	var options securityOptions
	for _, requirement := range requirements {
		if len(requirement.Schemes) > maxListItems {
			return securityOptions{}, errors.New("invalid A2A security requirement")
		}
		if len(requirement.Schemes) == 0 {
			options.public = true
			continue
		}
		bearerAlternative := len(requirement.Schemes) == 1
		for name, scopes := range requirement.Schemes {
			if name == "" || len(name) > maxStringBytes || len(scopes.List) > maxListItems || !validateStrings(scopes.List, false) {
				return securityOptions{}, errors.New("invalid A2A security requirement")
			}
			scheme, exists := card.SecuritySchemes[name]
			if !exists {
				return securityOptions{}, errors.New("unknown A2A security scheme")
			}
			if len(scopes.List) != 0 || len(scheme.HTTPAuth) == 0 || len(scheme.APIKey) != 0 || len(scheme.OAuth2) != 0 || len(scheme.OpenID) != 0 || len(scheme.MTLS) != 0 {
				bearerAlternative = false
				continue
			}
			var httpAuth httpAuthSecurityScheme
			if json.Unmarshal(scheme.HTTPAuth, &httpAuth) != nil || !strings.EqualFold(httpAuth.Scheme, "Bearer") {
				bearerAlternative = false
			}
		}
		options.bearer = options.bearer || bearerAlternative
	}
	return options, nil
}

func bearerPolicyForRequirements(card agentCard, requirements []agentSecurityRequirement, _ string) (bool, error) {
	options, err := validateSecurityRequirements(card, requirements)
	if err != nil {
		return false, err
	}
	if options.public {
		return false, nil
	}
	if !options.bearer {
		return false, errors.New("unsupported A2A authentication requirement")
	}
	return true, nil
}

func cardBearerPolicy(card agentCard, providerName string) (bool, error) {
	return bearerPolicyForRequirements(card, card.SecurityRequirements, providerName)
}

func readBoundedJSON(response *http.Response, allowedTypes ...string) ([]byte, error) {
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("A2A HTTP status %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return nil, errors.New("invalid A2A content type")
	}
	allowed := false
	for _, candidate := range allowedTypes {
		allowed = allowed || strings.EqualFold(mediaType, candidate)
	}
	if !allowed {
		return nil, errors.New("unsupported A2A content type")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, errors.New("A2A response exceeds limit")
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, errors.New("invalid A2A JSON response")
	}
	return body, nil
}

func validateStrings(values []string, required bool) bool {
	if (required && len(values) == 0) || len(values) > maxListItems {
		return false
	}
	for _, value := range values {
		if value == "" || len(value) > maxStringBytes || !utf8.ValidString(value) {
			return false
		}
	}
	return true
}

func validateCard(card agentCard) error {
	if card.Name == "" || card.Description == "" || card.Version == "" || len(card.Name) > maxStringBytes || len(card.Description) > maxStringBytes || len(card.Version) > maxStringBytes {
		return errors.New("invalid A2A AgentCard identity")
	}
	if len(card.Capabilities) == 0 || !validJSONObject(card.Capabilities) || len(card.SupportedInterfaces) == 0 || len(card.SupportedInterfaces) > maxListItems || len(card.Skills) == 0 || len(card.Skills) > maxSkills {
		return errors.New("invalid A2A AgentCard required fields")
	}
	capabilities, err := parseAgentCapabilities(card.Capabilities)
	if err != nil {
		return err
	}
	for _, extension := range capabilities.Extensions {
		if extension.Required {
			return errUnsupportedRequiredExtension
		}
	}
	if !validateStrings(card.DefaultInputModes, true) || !validateStrings(card.DefaultOutputModes, true) {
		return errors.New("invalid A2A AgentCard modes")
	}
	for _, skill := range card.Skills {
		if skill.ID == "" || skill.Name == "" || skill.Description == "" || len(skill.ID) > maxStringBytes || len(skill.Name) > maxStringBytes || len(skill.Description) > maxStringBytes || !validateStrings(skill.Tags, true) || !validateStrings(skill.Examples, false) || !validateStrings(skill.InputModes, false) || !validateStrings(skill.OutputModes, false) || len(skill.SecurityRequirements) > maxListItems {
			return errors.New("invalid A2A AgentSkill")
		}
	}
	if _, err := validateSecurityRequirements(card, card.SecurityRequirements); err != nil {
		return err
	}
	return nil
}

func parseAgentCapabilities(raw json.RawMessage) (agentCapabilities, error) {
	var capabilities agentCapabilities
	if err := json.Unmarshal(raw, &capabilities); err != nil || len(capabilities.Extensions) > maxListItems {
		return agentCapabilities{}, errors.New("invalid A2A AgentCapabilities")
	}
	seen := make(map[string]struct{}, len(capabilities.Extensions))
	for _, extension := range capabilities.Extensions {
		uri, err := url.ParseRequestURI(extension.URI)
		if err != nil || extension.URI == "" || !uri.IsAbs() || len(extension.URI) > maxStringBytes || !utf8.ValidString(extension.URI) || len(extension.Description) > maxStringBytes || !utf8.ValidString(extension.Description) || !validJSONObject(extension.Params) {
			return agentCapabilities{}, errors.New("invalid A2A AgentExtension")
		}
		if _, duplicate := seen[extension.URI]; duplicate {
			return agentCapabilities{}, errors.New("duplicate A2A AgentExtension URI")
		}
		seen[extension.URI] = struct{}{}
	}
	return capabilities, nil
}

func (a *Adapter) resolve(ctx context.Context, p provider.Provider) (agentCard, resolvedProvider, error) {
	if err := validateProvider(p); err != nil {
		return agentCard{}, resolvedProvider{}, adapterError("invalid_provider", "invalid A2A provider configuration")
	}
	cardURL, err := discoveryURL(p.Endpoint)
	if err != nil {
		return agentCard{}, resolvedProvider{}, adapterError("invalid_provider", "invalid A2A provider endpoint")
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, cardURL.String(), nil)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("A2A-Version", protocolVersion)
	response, err := a.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return agentCard{}, resolvedProvider{}, ctx.Err()
		}
		return agentCard{}, resolvedProvider{}, adapterError("a2a_unavailable", "A2A discovery endpoint is unavailable")
	}
	body, err := readBoundedJSON(response, "application/json", "application/a2a+json")
	closeErr := response.Body.Close()
	if err != nil {
		return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", "A2A discovery returned an invalid response")
	}
	if closeErr != nil {
		return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", "A2A discovery returned an invalid response")
	}
	var card agentCard
	if err := json.Unmarshal(body, &card); err != nil {
		return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", "invalid A2A AgentCard")
	}
	if err := validateCard(card); err != nil {
		return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", err.Error())
	}
	authBySkill := make(map[string]bool, len(card.Skills))
	for _, skill := range card.Skills {
		requirements := card.SecurityRequirements
		if len(skill.SecurityRequirements) != 0 {
			requirements = skill.SecurityRequirements
		}
		bearer, policyErr := bearerPolicyForRequirements(card, requirements, p.Name)
		if policyErr != nil {
			return agentCard{}, resolvedProvider{}, adapterError("a2a_auth", policyErr.Error())
		}
		authBySkill[skill.ID] = bearer
	}
	for _, bearer := range authBySkill {
		if bearer && p.Config["credential_reference"] == "" {
			return agentCard{}, resolvedProvider{}, adapterError("a2a_auth", "A2A manifest must declare one bearer credential")
		}
	}
	for _, candidate := range card.SupportedInterfaces {
		if candidate.ProtocolBinding != "HTTP+JSON" || candidate.ProtocolVersion != protocolVersion {
			continue
		}
		if len(candidate.URL) == 0 || len(candidate.URL) > maxStringBytes {
			return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", "invalid A2A HTTP+JSON interface")
		}
		operationURL, parseErr := url.Parse(candidate.URL)
		if parseErr != nil || (operationURL.Scheme != "http" && operationURL.Scheme != "https") || operationURL.Host == "" || operationURL.User != nil || operationURL.Fragment != "" || operationURL.RawQuery != "" || len(candidate.Tenant) > maxStringBytes {
			return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", "invalid A2A HTTP+JSON interface")
		}
		if cardURL.Scheme == "https" && operationURL.Scheme != "https" {
			return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", "A2A HTTP+JSON interface downgraded HTTPS")
		}
		if err := validateTransportURL(operationURL); err != nil {
			return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", "invalid A2A HTTP+JSON transport")
		}
		if canonicalOrigin(cardURL) != canonicalOrigin(operationURL) {
			return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", "A2A HTTP+JSON interface changed origin")
		}
		resolved := resolvedProvider{baseURL: strings.TrimRight(operationURL.String(), "/"), tenant: candidate.Tenant, authBySkill: authBySkill}
		a.mu.Lock()
		a.cache[p.ID] = resolved
		a.mu.Unlock()
		return card, resolved, nil
	}
	return agentCard{}, resolvedProvider{}, adapterError("invalid_agent_card", "A2A AgentCard has no HTTP+JSON 1.0 interface")
}

func (a *Adapter) Discover(ctx context.Context, p provider.Provider) ([]capability.Capability, error) {
	card, resolved, err := a.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]capability.Capability, 0, len(card.Skills))
	capabilities, _ := parseAgentCapabilities(card.Capabilities)
	seenIDs := make(map[string]struct{}, len(card.Skills))
	for _, skill := range card.Skills {
		if capability.Slug(skill.ID) == "" {
			return nil, adapterError("invalid_agent_card", "A2A skill ID must map to a non-empty capability reference")
		}
		id := capability.StableID("a2a", p.Name, skill.ID)
		if _, exists := seenIDs[id]; exists {
			return nil, adapterError("invalid_agent_card", "A2A AgentCard skills map to duplicate capability IDs")
		}
		seenIDs[id] = struct{}{}
		raw, _ := json.Marshal(skill)
		if len(capabilities.Extensions) != 0 {
			raw, _ = json.Marshal(struct {
				Skill      agentSkill       `json:"skill"`
				Extensions []agentExtension `json:"extensions"`
			}{Skill: skill, Extensions: capabilities.Extensions})
		}
		inputModes := skill.InputModes
		if len(inputModes) == 0 {
			inputModes = card.DefaultInputModes
		}
		outputModes := skill.OutputModes
		if len(outputModes) == 0 {
			outputModes = card.DefaultOutputModes
		}
		upstreamAuth := "none"
		if resolved.authBySkill[skill.ID] {
			upstreamAuth = "secret"
		}
		out = append(out, capability.Capability{
			ID: id, Kind: "a2a.skill", Name: skill.Name, Description: skill.Description,
			Source:    capability.Source{Protocol: "a2a", Provider: p.Name, UpstreamName: skill.ID},
			Input:     capability.Contract{Mode: "json", MediaTypes: inputModes, JSONSchema: invokeInputSchema()},
			Output:    capability.Contract{Mode: "a2a.response", MediaTypes: outputModes, JSONSchema: map[string]any{}},
			Lifecycle: capability.Lifecycle{Sync: true, Cancelable: true},
			Security:  capability.Security{RequiresApproval: "always", UpstreamAuth: upstreamAuth},
			Tags:      append([]string(nil), skill.Tags...), Examples: append([]string(nil), skill.Examples...), RawMetadata: raw,
		})
	}
	return out, nil
}

func adapterError(code, message string) error {
	err, _ := provider.NewAdapterError(code, message)
	return err
}

func (a *Adapter) Cancel(ctx context.Context, p provider.Provider, invocationID string) error {
	if err := validateProvider(p); err != nil {
		return err
	}
	a.mu.Lock()
	active := a.active[invocationID]
	if active != nil && active.providerID == p.ID && !active.canceling {
		active.canceling = true
	} else {
		active = nil
	}
	a.mu.Unlock()
	if active == nil {
		return adapterError("not_cancelable", "A2A invocation is not cancelable")
	}
	active.terminalMu.Lock()
	defer active.terminalMu.Unlock()
	confirmed := false
	defer func() {
		if confirmed {
			return
		}
		a.mu.Lock()
		if a.active[invocationID] == active {
			active.canceling = false
		}
		a.mu.Unlock()
	}()
	endpoint := taskEndpoint(a.cachedBaseURL(p.ID), active.taskID, "", ":cancel")
	if strings.HasPrefix(endpoint, "/tasks/") {
		return adapterError("provider_unavailable", "A2A provider is not resolved")
	}
	a.mu.Lock()
	stillActive := a.active[invocationID] == active && active.canceling
	a.mu.Unlock()
	if !stillActive {
		return adapterError("not_cancelable", "A2A invocation is no longer active")
	}
	body, err := a.operationJSON(ctx, p, active.bearer, http.MethodPost, endpoint, struct {
		Tenant string `json:"tenant,omitempty"`
	}{Tenant: active.tenant})
	if err != nil {
		return err
	}
	_, status, err := parseTask(body, active.taskID)
	if err != nil {
		return adapterError("invalid_response", "invalid A2A cancel response")
	}
	if status.State != "TASK_STATE_CANCELED" {
		return adapterError("invalid_response", "A2A cancel was not confirmed")
	}
	a.mu.Lock()
	if a.active[invocationID] != active || !active.canceling {
		a.mu.Unlock()
		return adapterError("not_cancelable", "A2A invocation is no longer active")
	}
	if len(active.updates) == cap(active.updates) {
		a.mu.Unlock()
		return errors.New("A2A cancellation update could not be delivered")
	}
	active.owner = terminalCancel
	delete(a.active, invocationID)
	active.updates <- append(json.RawMessage(nil), body...)
	confirmed = true
	active.cancel()
	a.mu.Unlock()
	return nil
}

func (a *Adapter) cachedBaseURL(providerID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cache[providerID].baseURL
}
func (a *Adapter) Health(ctx context.Context, p provider.Provider) provider.Health {
	if _, err := a.cachedOrResolve(ctx, p); err != nil {
		return provider.Health{Message: "A2A provider unavailable"}
	}
	return provider.Health{Healthy: true, Message: "ok"}
}
func (a *Adapter) Close(_ context.Context, p provider.Provider) error {
	if err := validateProvider(p); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.cache, p.ID)
	var cancels []context.CancelFunc
	for _, active := range a.active {
		if active.providerID == p.ID {
			cancels = append(cancels, active.cancel)
		}
	}
	a.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return nil
}
