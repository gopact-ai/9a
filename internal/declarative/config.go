package declarative

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"
)

const MaxSourceBytes = 8 << 20

var (
	namePattern     = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	templatePattern = regexp.MustCompile(`\{\{\s*([a-zA-Z][a-zA-Z0-9_-]*)\.([a-zA-Z0-9_.-]+)\s*\}\}`)
	envPattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type Config struct {
	APIVersion string               `yaml:"apiVersion"`
	Kind       string               `yaml:"kind"`
	Metadata   Metadata             `yaml:"metadata"`
	Projection Projection           `yaml:"projection,omitempty"`
	Variables  map[string]Variable  `yaml:"variables,omitempty"`
	Services   map[string]Service   `yaml:"services"`
	Operations map[string]Operation `yaml:"operations"`
	Workflows  map[string]Workflow  `yaml:"workflows,omitempty"`
	Security   Security             `yaml:"security,omitempty"`
	Digest     string               `yaml:"-"`
	Source     []byte               `yaml:"-"`
}

type Metadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

type Projection struct {
	Targets []string `yaml:"targets,omitempty"`
}

type Variable struct {
	FromEnv   string `yaml:"fromEnv,omitempty"`
	Default   string `yaml:"default,omitempty"`
	Sensitive bool   `yaml:"sensitive,omitempty"`
	Required  bool   `yaml:"required,omitempty"`
}

type Service struct {
	BaseURL string            `yaml:"baseURL"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Timeout string            `yaml:"timeout,omitempty"`
}

type Operation struct {
	Description  string         `yaml:"description,omitempty"`
	Service      string         `yaml:"service"`
	Method       string         `yaml:"method"`
	Path         string         `yaml:"path"`
	Request      RequestMapping `yaml:"request,omitempty"`
	InputSchema  map[string]any `yaml:"inputSchema,omitempty"`
	OutputSchema map[string]any `yaml:"outputSchema,omitempty"`
	Hooks        Hooks          `yaml:"hooks,omitempty"`
}

type RequestMapping struct {
	Query   map[string]any    `yaml:"query,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Body    any               `yaml:"body,omitempty"`
}

type Hooks struct {
	BeforeRequest []Hook `yaml:"beforeRequest,omitempty"`
	AfterResponse []Hook `yaml:"afterResponse,omitempty"`
}

type Hook struct {
	SetHeaders    map[string]string `yaml:"setHeaders,omitempty"`
	RemoveHeaders []string          `yaml:"removeHeaders,omitempty"`
	Transform     *Transform        `yaml:"transform,omitempty"`
	Exec          *ExecHook         `yaml:"exec,omitempty"`
}

type Transform struct {
	Language   string `yaml:"language"`
	Expression string `yaml:"expression"`
}

type ExecHook struct {
	Command        []string `yaml:"command"`
	Env            []string `yaml:"env,omitempty"`
	Timeout        string   `yaml:"timeout,omitempty"`
	MaxOutputBytes int64    `yaml:"maxOutputBytes,omitempty"`
}

type Workflow struct {
	Description  string         `yaml:"description,omitempty"`
	InputSchema  map[string]any `yaml:"inputSchema,omitempty"`
	OutputSchema map[string]any `yaml:"outputSchema,omitempty"`
	Steps        []Step         `yaml:"steps"`
	Output       *Transform     `yaml:"output,omitempty"`
}

type Step struct {
	ID    string         `yaml:"id"`
	Use   string         `yaml:"use"`
	Input map[string]any `yaml:"input,omitempty"`
}

type Security struct {
	AllowExecutableHooks bool `yaml:"allowExecutableHooks,omitempty"`
}

func Parse(source []byte) (*Config, error) {
	if len(source) == 0 {
		return nil, errors.New("source is empty")
	}
	if len(source) > MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", MaxSourceBytes)
	}
	if !utf8.Valid(source) {
		return nil, errors.New("source is not valid UTF-8")
	}

	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	if err := validateYAMLNode(&document); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("source must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode YAML: %w", err)
	}

	var config Config
	strict := yaml.NewDecoder(bytes.NewReader(source))
	strict.KnownFields(true)
	if err := strict.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("canonicalize source: %w", err)
	}
	digest := sha256.Sum256(canonical)
	config.Digest = hex.EncodeToString(digest[:])
	config.Source = append([]byte(nil), source...)
	return &config, nil
}

func validateYAMLNode(node *yaml.Node) error {
	if node.Alias != nil || node.Kind == yaml.AliasNode {
		return fmt.Errorf("YAML aliases are not allowed at line %d", node.Line)
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("mapping keys must be strings at line %d", key.Line)
			}
			if _, ok := seen[key.Value]; ok {
				return fmt.Errorf("duplicate key %q at line %d", key.Value, key.Line)
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := validateYAMLNode(child); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) SkillRoot() string {
	if len(c.Projection.Targets) > 0 {
		return c.Projection.Targets[0]
	}
	return ".agents/skills"
}

func (c *Config) Validate() error {
	if c.APIVersion != "9a.dev/v1alpha1" {
		return fmt.Errorf("apiVersion must be 9a.dev/v1alpha1")
	}
	if c.Kind != "Skill" {
		return fmt.Errorf("kind must be Skill")
	}
	if err := validateName("metadata.name", c.Metadata.Name); err != nil {
		return err
	}
	if len(c.Projection.Targets) > 1 {
		return errors.New("projection.targets currently supports one target")
	}
	for _, target := range c.Projection.Targets {
		clean := filepath.Clean(target)
		if filepath.IsAbs(target) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("projection target %q must be a relative directory", target)
		}
	}
	for name, variable := range c.Variables {
		if err := validateName("variable", name); err != nil {
			return err
		}
		if variable.FromEnv == "" && variable.Default == "" {
			return fmt.Errorf("variable %q requires fromEnv or default", name)
		}
		if variable.FromEnv != "" && !envPattern.MatchString(variable.FromEnv) {
			return fmt.Errorf("variable %q has invalid fromEnv %q", name, variable.FromEnv)
		}
	}
	if len(c.Services) == 0 {
		return errors.New("at least one service is required")
	}
	for name, service := range c.Services {
		if err := validateName("service", name); err != nil {
			return err
		}
		if err := validateBaseURL(service.BaseURL); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if service.Timeout != "" {
			if err := validateDuration(service.Timeout, 5*time.Minute); err != nil {
				return fmt.Errorf("service %q timeout: %w", name, err)
			}
		}
		if err := c.validateTemplates(service.Headers, nil); err != nil {
			return fmt.Errorf("service %q headers: %w", name, err)
		}
		if err := validateHeaderMap(service.Headers); err != nil {
			return fmt.Errorf("service %q headers: %w", name, err)
		}
	}
	if len(c.Operations) == 0 {
		return errors.New("at least one operation is required")
	}
	for name, operation := range c.Operations {
		if err := validateName("operation", name); err != nil {
			return err
		}
		if _, ok := c.Services[operation.Service]; !ok {
			return fmt.Errorf("operation %q references unknown service %q", name, operation.Service)
		}
		if !allowedMethod(operation.Method) {
			return fmt.Errorf("operation %q has unsupported method %q", name, operation.Method)
		}
		if err := validateOperationPath(operation.Path); err != nil {
			return fmt.Errorf("operation %q: %w", name, err)
		}
		if err := c.validateTemplates(operation.Request, nil); err != nil {
			return fmt.Errorf("operation %q request: %w", name, err)
		}
		if err := validateHeaderMap(operation.Request.Headers); err != nil {
			return fmt.Errorf("operation %q headers: %w", name, err)
		}
		if err := c.validateHooks(operation.Hooks); err != nil {
			return fmt.Errorf("operation %q: %w", name, err)
		}
	}
	for name, workflow := range c.Workflows {
		if err := validateName("workflow", name); err != nil {
			return err
		}
		if len(workflow.Steps) == 0 {
			return fmt.Errorf("workflow %q requires at least one step", name)
		}
		prior := map[string]struct{}{}
		for _, step := range workflow.Steps {
			if err := validateName("workflow step", step.ID); err != nil {
				return err
			}
			if _, exists := prior[step.ID]; exists {
				return fmt.Errorf("workflow %q has duplicate step %q", name, step.ID)
			}
			if _, ok := c.Operations[step.Use]; !ok {
				return fmt.Errorf("workflow %q step %q references unknown operation %q", name, step.ID, step.Use)
			}
			if err := c.validateTemplates(step.Input, prior); err != nil {
				return fmt.Errorf("workflow %q step %q: %w", name, step.ID, err)
			}
			prior[step.ID] = struct{}{}
		}
		if workflow.Output != nil {
			if err := validateTransform(workflow.Output); err != nil {
				return fmt.Errorf("workflow %q output: %w", name, err)
			}
		}
	}
	return nil
}

func validateName(field, value string) error {
	if !namePattern.MatchString(value) {
		return fmt.Errorf("%s %q must match %s", field, value, namePattern)
	}
	return nil
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("baseURL %q is invalid", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("baseURL cannot contain credentials, query, or fragment")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return errors.New("baseURL must use HTTPS, or HTTP for a loopback host")
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("remote baseURL must use HTTPS")
		}
	}
	return nil
}

func validateOperationPath(raw string) error {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return errors.New("path must be root-relative")
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("path cannot contain a host, query, or fragment")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == ".." {
			return errors.New("path cannot contain ..")
		}
	}
	return nil
}

func allowedMethod(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

func (c *Config) validateHooks(hooks Hooks) error {
	for phase, list := range map[string][]Hook{"beforeRequest": hooks.BeforeRequest, "afterResponse": hooks.AfterResponse} {
		for i, hook := range list {
			actions := 0
			if hook.SetHeaders != nil {
				actions++
			}
			if hook.RemoveHeaders != nil {
				actions++
			}
			if hook.Transform != nil {
				actions++
			}
			if hook.Exec != nil {
				actions++
			}
			if actions != 1 {
				return fmt.Errorf("%s hook %d must contain exactly one action", phase, i)
			}
			if phase == "afterResponse" && (hook.SetHeaders != nil || hook.RemoveHeaders != nil) {
				return fmt.Errorf("%s hook %d only supports transform or exec", phase, i)
			}
			if err := c.validateTemplates(hook.SetHeaders, nil); err != nil {
				return err
			}
			if err := validateHeaderMap(hook.SetHeaders); err != nil {
				return err
			}
			for _, name := range hook.RemoveHeaders {
				if !validHeaderName(name) {
					return fmt.Errorf("invalid header name %q", name)
				}
			}
			if hook.Transform != nil {
				if err := validateTransform(hook.Transform); err != nil {
					return err
				}
			}
			if hook.Exec != nil {
				if !c.Security.AllowExecutableHooks {
					return errors.New("executable hooks require security.allowExecutableHooks: true")
				}
				if len(hook.Exec.Command) == 0 || !filepath.IsAbs(hook.Exec.Command[0]) {
					return errors.New("executable hook command must start with an absolute path")
				}
				if len(hook.Exec.Command) > 64 {
					return errors.New("executable hook command has too many arguments")
				}
				for _, name := range hook.Exec.Env {
					if !envPattern.MatchString(name) {
						return fmt.Errorf("invalid executable hook environment name %q", name)
					}
				}
				if hook.Exec.Timeout != "" {
					if err := validateDuration(hook.Exec.Timeout, 30*time.Second); err != nil {
						return fmt.Errorf("executable hook timeout: %w", err)
					}
				}
				if hook.Exec.MaxOutputBytes < 0 || hook.Exec.MaxOutputBytes > MaxSourceBytes {
					return fmt.Errorf("executable hook maxOutputBytes must be between 0 and %d", MaxSourceBytes)
				}
			}
		}
	}
	return nil
}

func validateHeaderMap(headers map[string]string) error {
	seen := make(map[string]struct{}, len(headers))
	for name := range headers {
		if !validHeaderName(name) {
			return fmt.Errorf("invalid header name %q", name)
		}
		canonical := strings.ToLower(name)
		if _, exists := seen[canonical]; exists {
			return fmt.Errorf("duplicate header name %q", name)
		}
		seen[canonical] = struct{}{}
	}
	return nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			continue
		}
		if strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	return true
}

func validateDuration(raw string, maximum time.Duration) error {
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return errors.New("must be a positive Go duration")
	}
	if duration > maximum {
		return fmt.Errorf("must not exceed %s", maximum)
	}
	return nil
}

func validateTransform(transform *Transform) error {
	if transform.Language != "jq" || strings.TrimSpace(transform.Expression) == "" {
		return errors.New("transform requires language: jq and a non-empty expression")
	}
	if _, err := gojq.Parse(transform.Expression); err != nil {
		return fmt.Errorf("invalid jq expression: %w", err)
	}
	return nil
}

func (c *Config) validateTemplates(value any, priorSteps map[string]struct{}) error {
	return walkStrings(value, func(text string) error {
		for _, match := range templatePattern.FindAllStringSubmatch(text, -1) {
			switch match[1] {
			case "input":
			case "vars":
				name := strings.Split(match[2], ".")[0]
				if _, ok := c.Variables[name]; !ok {
					return fmt.Errorf("references undeclared variable %q", name)
				}
			case "steps":
				name := strings.Split(match[2], ".")[0]
				if priorSteps == nil {
					return fmt.Errorf("step reference %q is not valid here", name)
				}
				if _, ok := priorSteps[name]; !ok {
					return fmt.Errorf("references unavailable step %q", name)
				}
			default:
				return fmt.Errorf("unknown template namespace %q", match[1])
			}
		}
		stripped := templatePattern.ReplaceAllString(text, "")
		if strings.Contains(stripped, "{{") || strings.Contains(stripped, "}}") {
			return errors.New("contains a malformed template expression")
		}
		return nil
	})
}

func walkStrings(value any, visit func(string) error) error {
	switch typed := value.(type) {
	case string:
		return visit(typed)
	case map[string]string:
		for key, item := range typed {
			if err := visit(key); err != nil {
				return err
			}
			if err := visit(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, item := range typed {
			if err := visit(key); err != nil {
				return err
			}
			if err := walkStrings(item, visit); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := walkStrings(item, visit); err != nil {
				return err
			}
		}
	case RequestMapping:
		if err := walkStrings(typed.Query, visit); err != nil {
			return err
		}
		if err := walkStrings(typed.Headers, visit); err != nil {
			return err
		}
		return walkStrings(typed.Body, visit)
	}
	return nil
}
