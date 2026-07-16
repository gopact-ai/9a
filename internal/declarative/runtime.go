package declarative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/9a/internal/jsoncontract"
	"github.com/gopact-ai/9a/internal/jsonvalue"
	"github.com/gopact-ai/9a/internal/processgroup"
	"github.com/gopact-ai/9a/internal/secret"
	"github.com/itchyny/gojq"
)

const maxResponseBytes = 8 << 20

var executableHookSlots = make(chan struct{}, 32)

func invokeOperation(ctx context.Context, operation Operation, service Service, input any, credentials *credentialValues) (any, error) {
	requestState := map[string]any{
		"input":   input,
		"query":   map[string]any{},
		"headers": map[string]any{},
		"body":    nil,
	}
	context := templateContext{"input": input, "secrets": credentials}
	query, err := resolveValue(operation.Request.Query, context)
	if err != nil {
		return nil, err
	}
	requestState["query"] = query
	body, err := resolveValue(operation.Request.Body, context)
	if err != nil {
		return nil, err
	}
	requestState["body"] = body
	headers := make(map[string]any)
	for _, source := range []map[string]string{service.Headers, operation.Request.Headers} {
		resolved, err := resolveValue(source, context)
		if err != nil {
			return nil, err
		}
		for key, value := range toStringMap(resolved) {
			headers[http.CanonicalHeaderKey(key)] = value
		}
	}
	requestState["headers"] = headers
	for _, hook := range operation.Hooks.BeforeRequest {
		requestState, err = runRequestHook(ctx, hook, requestState, context)
		if err != nil {
			return nil, fmt.Errorf("beforeRequest hook: %w", err)
		}
	}

	endpoint, err := url.Parse(strings.TrimRight(service.BaseURL, "/") + operation.Path)
	if err != nil {
		return nil, err
	}
	queryState := requestState["query"]
	queryValues := map[string]any{}
	if queryState != nil {
		var ok bool
		queryValues, ok = queryState.(map[string]any)
		if !ok {
			return nil, errors.New("request query must be an object")
		}
	}
	for key, value := range queryValues {
		values := endpoint.Query()
		appendQuery(values, key, value)
		endpoint.RawQuery = values.Encode()
	}
	var requestBody io.Reader
	if requestState["body"] != nil {
		encoded, err := json.Marshal(requestState["body"])
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, strings.ToUpper(operation.Method), endpoint.String(), requestBody)
	if err != nil {
		return nil, err
	}
	for key, value := range toAnyMap(requestState["headers"]) {
		request.Header.Set(key, fmt.Sprint(value))
	}
	if requestState["body"] != nil && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	timeout := 30 * time.Second
	if service.Timeout != "" {
		parsed, err := time.ParseDuration(service.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid service timeout: %w", err)
		}
		timeout = parsed
	}
	originScheme, originHost := request.URL.Scheme, request.URL.Host
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("HTTP redirect limit exceeded")
			}
			if next.URL.Scheme != originScheme || next.URL.Host != originHost {
				return errors.New("HTTP redirect changed origin")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("HTTP request failed")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	closeErr := response.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read HTTP response: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close HTTP response: %w", closeErr)
	}
	if len(raw) > maxResponseBytes {
		return nil, errors.New("HTTP response exceeds 8 MiB")
	}
	var responseBody any
	if len(raw) > 0 && strings.Contains(response.Header.Get("Content-Type"), "json") {
		if err := jsonvalue.Decode(raw, &responseBody); err != nil {
			return nil, fmt.Errorf("decode JSON response: %w", err)
		}
	} else {
		responseBody = string(raw)
	}
	responseState := any(map[string]any{
		"status":  response.StatusCode,
		"headers": response.Header,
		"body":    responseBody,
	})
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP response status %d", response.StatusCode)
	}
	for _, hook := range operation.Hooks.AfterResponse {
		responseState, err = runResponseHook(ctx, hook, responseState, context)
		if err != nil {
			return nil, fmt.Errorf("afterResponse hook: %w", err)
		}
	}
	return responseState, nil
}

func invokeWorkflow(ctx context.Context, config *Config, workflow Workflow, input any, credentials *credentialValues) (any, error) {
	steps := make(map[string]any, len(workflow.Steps))
	for _, step := range workflow.Steps {
		resolved, err := resolveValue(step.Input, templateContext{"input": input, "secrets": credentials, "steps": steps})
		if err != nil {
			return nil, fmt.Errorf("workflow step %q input: %w", step.ID, err)
		}
		operation := config.Capabilities[step.Use]
		encodedInput, err := json.Marshal(resolved)
		if err != nil {
			return nil, fmt.Errorf("workflow step %q input: encode: %w", step.ID, err)
		}
		if err := jsoncontract.Validate(operation.InputSchema, encodedInput); err != nil {
			return nil, fmt.Errorf("workflow step %q input: %w", step.ID, err)
		}
		result, err := invokeOperation(ctx, operation, config.Services[operation.Service], resolved, credentials)
		if err != nil {
			return nil, fmt.Errorf("workflow step %q: %w", step.ID, err)
		}
		encodedOutput, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("workflow step %q output: encode: %w", step.ID, err)
		}
		if err := jsoncontract.Validate(operation.OutputSchema, encodedOutput); err != nil {
			return nil, fmt.Errorf("workflow step %q output: %w", step.ID, err)
		}
		steps[step.ID] = result
	}
	state := map[string]any{"input": input, "steps": steps}
	if workflow.Output == nil {
		return state, nil
	}
	return runJQ(workflow.Output.Expression, state)
}

type templateContext map[string]any

type credentialValues struct {
	ctx         context.Context
	resolver    secret.Resolver
	credentials map[string]Credential
	values      map[string]string
}

func newCredentialValues(ctx context.Context, resolver secret.Resolver, credentials map[string]Credential) *credentialValues {
	return &credentialValues{ctx: ctx, resolver: resolver, credentials: credentials, values: make(map[string]string)}
}

func (v *credentialValues) resolve(alias string) (string, error) {
	credential, ok := v.credentials[alias]
	if !ok {
		return "", fmt.Errorf("secret alias %q is undeclared", alias)
	}
	if value, ok := v.values[alias]; ok {
		return value, nil
	}
	if v.resolver == nil {
		return "", &secret.MissingError{Reference: credential.Secret}
	}
	value, err := v.resolver.Resolve(v.ctx, credential.Secret)
	if err != nil {
		return "", fmt.Errorf("resolve secret %q: %w", credential.Secret, err)
	}
	if value == "" {
		return "", &secret.MissingError{Reference: credential.Secret}
	}
	v.values[alias] = value
	return value, nil
}

func resolveValue(value any, context templateContext) (any, error) {
	switch typed := value.(type) {
	case string:
		matches := templatePattern.FindAllStringSubmatchIndex(typed, -1)
		if len(matches) == 0 {
			return typed, nil
		}
		if len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(typed) {
			return lookupTemplate(context, typed[matches[0][2]:matches[0][3]], typed[matches[0][4]:matches[0][5]])
		}
		result := typed
		for i := len(matches) - 1; i >= 0; i-- {
			match := matches[i]
			resolved, err := lookupTemplate(context, typed[match[2]:match[3]], typed[match[4]:match[5]])
			if err != nil {
				return nil, err
			}
			result = result[:match[0]] + fmt.Sprint(resolved) + result[match[1]:]
		}
		return result, nil
	case map[string]string:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			resolved, err := resolveValue(item, context)
			if err != nil {
				return nil, err
			}
			result[key] = resolved
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			resolved, err := resolveValue(item, context)
			if err != nil {
				return nil, err
			}
			result[key] = resolved
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for i, item := range typed {
			resolved, err := resolveValue(item, context)
			if err != nil {
				return nil, err
			}
			result[i] = resolved
		}
		return result, nil
	default:
		return value, nil
	}
}

func lookupTemplate(context templateContext, namespace, path string) (any, error) {
	current, ok := context[namespace]
	if !ok {
		return nil, fmt.Errorf("template namespace %q is unavailable", namespace)
	}
	if credentials, ok := current.(*credentialValues); ok {
		return credentials.resolve(path)
	}
	for _, segment := range strings.Split(path, ".") {
		switch typed := current.(type) {
		case map[string]any:
			current, ok = typed[segment]
		case map[string]string:
			current, ok = typed[segment]
		default:
			reflected := reflect.ValueOf(current)
			if reflected.IsValid() && reflected.Kind() == reflect.Map {
				item := reflected.MapIndex(reflect.ValueOf(segment))
				ok = item.IsValid()
				if ok {
					current = item.Interface()
				}
			} else {
				ok = false
			}
		}
		if !ok {
			return nil, fmt.Errorf("template value %s.%s is missing", namespace, path)
		}
	}
	return current, nil
}

func runRequestHook(ctx context.Context, hook Hook, state map[string]any, templates templateContext) (map[string]any, error) {
	if hook.SetHeaders != nil {
		resolved, err := resolveValue(hook.SetHeaders, templates)
		if err != nil {
			return nil, err
		}
		headers := toAnyMap(state["headers"])
		for key, value := range toAnyMap(resolved) {
			headers[http.CanonicalHeaderKey(key)] = value
		}
		state["headers"] = headers
	}
	if hook.RemoveHeaders != nil {
		headers := toAnyMap(state["headers"])
		for _, key := range hook.RemoveHeaders {
			delete(headers, http.CanonicalHeaderKey(key))
		}
		state["headers"] = headers
	}
	if hook.Transform != nil {
		value, err := runJQ(hook.Transform.Expression, state)
		if err != nil {
			return nil, err
		}
		mapped, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("request transform must return an object")
		}
		state = mapped
	}
	if hook.Exec != nil {
		value, err := runExecutableHook(ctx, *hook.Exec, state)
		if err != nil {
			return nil, err
		}
		mapped, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("request executable hook must return an object")
		}
		state = mapped
	}
	if err := normalizeRequestHeaders(state); err != nil {
		return nil, err
	}
	return state, nil
}

func normalizeRequestHeaders(state map[string]any) error {
	raw, exists := state["headers"]
	if !exists || raw == nil {
		state["headers"] = map[string]any{}
		return nil
	}
	headers, ok := raw.(map[string]any)
	if !ok {
		return errors.New("request headers must be an object")
	}
	normalized := make(map[string]any, len(headers))
	seen := make(map[string]struct{}, len(headers))
	for name, value := range headers {
		if !validHeaderName(name) {
			return errors.New("request hook produced an invalid header name")
		}
		key := strings.ToLower(name)
		if _, duplicate := seen[key]; duplicate {
			return errors.New("request hook produced duplicate header names")
		}
		seen[key] = struct{}{}
		normalized[http.CanonicalHeaderKey(name)] = value
	}
	state["headers"] = normalized
	return nil
}

func runResponseHook(ctx context.Context, hook Hook, state any, _ templateContext) (any, error) {
	if hook.Transform != nil {
		return runJQ(hook.Transform.Expression, state)
	}
	if hook.Exec != nil {
		return runExecutableHook(ctx, *hook.Exec, state)
	}
	return state, nil
}

func runExecutableHook(ctx context.Context, hook ExecHook, state any) (any, error) {
	select {
	case executableHookSlots <- struct{}{}:
		defer func() { <-executableHookSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errors.New("executable hook capacity exhausted")
	}
	timeout := 5 * time.Second
	if hook.Timeout != "" {
		parsed, err := time.ParseDuration(hook.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid executable hook timeout: %w", err)
		}
		timeout = parsed
	}
	maxOutput := hook.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = 1 << 20
	}
	hookContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	input, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode executable hook input: %w", err)
	}
	command := exec.Command(hook.Command[0], hook.Command[1:]...)
	processgroup.Configure(command)
	command.Env = make([]string, 0, len(hook.Env))
	for _, name := range hook.Env {
		if value, ok := os.LookupEnv(name); ok {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	command.Stdin = bytes.NewReader(input)
	kill := func() { _ = processgroup.Kill(command) }
	stdout := &boundedBuffer{limit: maxOutput, onLimit: kill}
	stderr := &boundedBuffer{limit: 64 << 10, onLimit: kill}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, errors.New("executable hook could not start")
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-hookContext.Done():
			_ = processgroup.Kill(command)
		case <-done:
		}
	}()
	err = command.Wait()
	close(done)
	if err != nil {
		if hookContext.Err() != nil {
			return nil, fmt.Errorf("executable hook: %w", hookContext.Err())
		}
		if stdout.exceeded || stderr.exceeded {
			return nil, errors.New("executable hook output exceeds configured limit")
		}
		return nil, errors.New("executable hook failed")
	}
	if stdout.exceeded {
		return nil, errors.New("executable hook output exceeds configured limit")
	}
	var result any
	if err := jsonvalue.Decode(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("decode executable hook output: %w", err)
	}
	return result, nil
}

type boundedBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
	onLimit  func()
	once     sync.Once
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.exceeded = true
		b.once.Do(b.onLimit)
		return len(data), nil
	}
	write := data
	if int64(len(write)) > remaining {
		write = write[:remaining]
		b.exceeded = true
		b.once.Do(b.onLimit)
	}
	_, _ = b.buffer.Write(write)
	return len(data), nil
}

func (b *boundedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func (b *boundedBuffer) String() string { return string(b.Bytes()) }

type exactJSONNumber string

func runJQ(expression string, value any) (result any, err error) {
	query, err := gojq.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("parse jq: %w", err)
	}
	value, hasExactNumber := protectJQNumbers(value)
	if hasExactNumber {
		defer func() {
			if recover() != nil {
				result = nil
				err = errors.New("jq cannot operate on a precision-sensitive JSON number")
			}
		}()
	}
	iterator := query.Run(value)
	result, ok := iterator.Next()
	if !ok {
		return nil, errors.New("jq produced no result")
	}
	if err, ok := result.(error); ok {
		_ = err
		if hasExactNumber {
			return nil, errors.New("jq cannot operate on a precision-sensitive JSON number")
		}
		return nil, errors.New("jq transform failed")
	}
	if _, more := iterator.Next(); more {
		return nil, errors.New("jq must produce exactly one result")
	}
	return restoreJQNumbers(result), nil
}

func protectJQNumbers(value any) (any, bool) {
	switch value := value.(type) {
	case json.Number:
		if jqCanRepresentNumber(value) {
			return value, false
		}
		return exactJSONNumber(value.String()), true
	case []any:
		result := make([]any, len(value))
		protected := false
		for i, item := range value {
			result[i], protected = protectJQNumber(item, protected)
		}
		return result, protected
	case map[string]any:
		result := make(map[string]any, len(value))
		protected := false
		for key, item := range value {
			result[key], protected = protectJQNumber(item, protected)
		}
		return result, protected
	default:
		return value, false
	}
}

func protectJQNumber(value any, alreadyProtected bool) (any, bool) {
	value, protected := protectJQNumbers(value)
	return value, alreadyProtected || protected
}

func jqCanRepresentNumber(number json.Number) bool {
	if !strings.ContainsAny(number.String(), ".eE") {
		return true // gojq represents arbitrary-size integers exactly.
	}
	exact, ok := new(big.Rat).SetString(number.String())
	if !ok {
		return false
	}
	value, err := number.Float64()
	if err != nil {
		return false
	}
	represented := new(big.Rat).SetFloat64(value)
	return represented != nil && exact.Cmp(represented) == 0
}

func restoreJQNumbers(value any) any {
	switch value := value.(type) {
	case exactJSONNumber:
		return json.Number(value)
	case *big.Int:
		return json.Number(value.String())
	case []any:
		for i, item := range value {
			value[i] = restoreJQNumbers(item)
		}
	case map[string]any:
		for key, item := range value {
			value[key] = restoreJQNumbers(item)
		}
	}
	return value
}

func appendQuery(values url.Values, key string, value any) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			values.Add(key, fmt.Sprint(item))
		}
	case []string:
		for _, item := range typed {
			values.Add(key, item)
		}
	case nil:
	default:
		values.Add(key, scalarString(typed))
	}
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(value)
	}
}

func toAnyMap(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	result := map[string]any{}
	reflected := reflect.ValueOf(value)
	if reflected.IsValid() && reflected.Kind() == reflect.Map {
		for _, key := range reflected.MapKeys() {
			result[fmt.Sprint(key.Interface())] = reflected.MapIndex(key).Interface()
		}
	}
	return result
}

func toStringMap(value any) map[string]string {
	result := map[string]string{}
	for key, item := range toAnyMap(value) {
		result[key] = fmt.Sprint(item)
	}
	return result
}
