package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxHTTPBodyBytes = 8 << 20
	maxHTTPURLBytes  = 16 << 10
)

type providerConfig struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

type adapterFault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	cause   error
}

func (e *adapterFault) Error() string { return e.Code + ": " + e.Message }

func safeFault(code, message string, cause error) *adapterFault {
	return &adapterFault{Code: code, Message: message, cause: cause}
}

type httpBridge struct {
	manifest *manifest
	client   *http.Client
}

func newHTTPBridge(configuration *manifest) *httpBridge {
	return &httpBridge{
		manifest: configuration,
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) > 3 {
					return errors.New("too many redirects")
				}
				if len(via) == 0 || canonicalOrigin(request.URL) != canonicalOrigin(via[0].URL) {
					return errors.New("redirect changed origin")
				}
				if via[0].URL.Scheme == "https" && request.URL.Scheme != "https" {
					return errors.New("redirect downgraded HTTPS")
				}
				return nil
			},
		},
	}
}

func (b *httpBridge) invoke(ctx context.Context, provider providerConfig, upstream string, input json.RawMessage) (json.RawMessage, *adapterFault) {
	operation, exists := b.manifest.byUpstream[upstream]
	if !exists {
		return nil, safeFault("unknown_operation", "HTTP operation is not configured", nil)
	}
	if len(input) > maxHTTPBodyBytes || !utf8.Valid(input) || !json.Valid(input) {
		return nil, safeFault("invalid_request", "invoke input must be bounded JSON", nil)
	}
	endpoint, fault := validateProvider(provider)
	if fault != nil {
		return nil, fault
	}
	requestURL := *endpoint
	requestURL.Path = operation.Path
	requestURL.RawPath = ""
	requestURL.RawQuery = ""
	requestURL.Fragment = ""
	var body io.Reader
	if operation.Method == http.MethodGet || operation.Method == http.MethodDelete {
		query, err := deterministicQuery(input)
		if err != nil {
			return nil, safeFault("invalid_request", "GET and DELETE input must be an object of scalar values", err)
		}
		requestURL.RawQuery = query
	} else {
		body = bytes.NewReader(input)
	}
	if len(requestURL.String()) > maxHTTPURLBytes {
		return nil, safeFault("invalid_request", "HTTP request URL exceeds limit", nil)
	}
	return b.do(ctx, provider, operation.Method, &requestURL, operation.Auth, body)
}

func (b *httpBridge) health(ctx context.Context, provider providerConfig) *adapterFault {
	if b.manifest.HealthPath == "" {
		if _, fault := validateProvider(provider); fault != nil {
			return fault
		}
		return nil
	}
	endpoint, fault := validateProvider(provider)
	if fault != nil {
		return fault
	}
	requestURL := *endpoint
	requestURL.Path = b.manifest.HealthPath
	requestURL.RawPath, requestURL.RawQuery, requestURL.Fragment = "", "", ""
	if len(requestURL.String()) > maxHTTPURLBytes {
		return safeFault("invalid_request", "HTTP request URL exceeds limit", nil)
	}
	_, fault = b.do(ctx, provider, http.MethodGet, &requestURL, b.manifest.HealthAuth, nil)
	return fault
}

func (b *httpBridge) do(ctx context.Context, provider providerConfig, method string, requestURL *url.URL, auth string, body io.Reader) (json.RawMessage, *adapterFault) {
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return nil, safeFault("invalid_request", "HTTP request could not be constructed", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if auth == "bearer" {
		token := os.Getenv(tokenEnvironmentName(provider.Name))
		if token == "" {
			return nil, safeFault("missing_credentials", "HTTP bearer token is required", nil)
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := b.client.Do(request)
	if err != nil {
		return nil, safeFault("upstream_unavailable", "HTTP upstream is unavailable", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, safeFault("upstream_error", "HTTP upstream returned an error status", nil)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return nil, safeFault("invalid_response", "HTTP upstream response is not JSON", err)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPBodyBytes+1))
	if err != nil {
		return nil, safeFault("upstream_unavailable", "HTTP upstream response could not be read", err)
	}
	if len(data) > maxHTTPBodyBytes {
		return nil, safeFault("response_too_large", "HTTP upstream response exceeds limit", nil)
	}
	if !utf8.Valid(data) || !json.Valid(data) {
		return nil, safeFault("invalid_response", "HTTP upstream returned invalid JSON", nil)
	}
	return append(json.RawMessage(nil), data...), nil
}

func validateProvider(provider providerConfig) (*url.URL, *adapterFault) {
	if !canonicalName.MatchString(provider.Name) || len(provider.Name) > maxManifestStringBytes {
		return nil, safeFault("invalid_request", "provider name must be a canonical slug", nil)
	}
	if provider.Endpoint == "" || len(provider.Endpoint) > maxHTTPURLBytes || !utf8.ValidString(provider.Endpoint) {
		return nil, safeFault("invalid_request", "provider endpoint is invalid", nil)
	}
	endpoint, err := url.Parse(provider.Endpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, safeFault("invalid_request", "provider endpoint is invalid", err)
	}
	if endpoint.Scheme == "http" && !loopbackHost(endpoint.Hostname()) {
		return nil, safeFault("invalid_request", "cleartext HTTP requires a loopback provider", nil)
	}
	return endpoint, nil
}

func loopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func canonicalOrigin(value *url.URL) string {
	host := strings.TrimSuffix(strings.ToLower(value.Hostname()), ".")
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := value.Port()
	if port == "" {
		if value.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(value.Scheme) + "://" + net.JoinHostPort(host, port)
}

func deterministicQuery(input json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return "", errors.New("input is not an object")
	}
	if err := ensureInputEOF(decoder); err != nil {
		return "", err
	}
	values := url.Values{}
	for key, value := range object {
		if key == "" || len(key) > maxManifestStringBytes || !utf8.ValidString(key) {
			return "", errors.New("invalid query key")
		}
		switch scalar := value.(type) {
		case nil:
			values.Set(key, "")
		case string:
			values.Set(key, scalar)
		case bool:
			values.Set(key, strconv.FormatBool(scalar))
		case json.Number:
			values.Set(key, scalar.String())
		default:
			return "", errors.New("query value is not scalar")
		}
	}
	return values.Encode(), nil
}

func ensureInputEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("input contains multiple JSON values")
	}
	return nil
}

func tokenEnvironmentName(providerName string) string {
	return "NINEA_HTTP_TOKEN_" + strings.ToUpper(strings.ReplaceAll(providerName, "-", "_"))
}
