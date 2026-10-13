// Package config loads and validates toll's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	FormatOpenAI    = "openai"
	FormatAnthropic = "anthropic"
	FormatGemini    = "gemini"

	OutputFile   = "file"
	OutputStdout = "stdout"

	FailOpen   = "fail_open"
	FailClosed = "fail_closed"
)

type Config struct {
	Server     Server              `yaml:"server"`
	Providers  map[string]Provider `yaml:"providers"`
	Metering   Metering            `yaml:"metering"`
	KeyAliases map[string]string   `yaml:"key_aliases"`
	Events     Events              `yaml:"events"`
}

type Server struct {
	Listen                        string        `yaml:"listen"`
	InstanceID                    string        `yaml:"instance_id"`
	AuthToken                     string        `yaml:"auth_token"`
	MaxRequestBody                ByteSize      `yaml:"max_request_body"`
	ShutdownGrace                 time.Duration `yaml:"shutdown_grace"`
	ReadHeaderTimeout             time.Duration `yaml:"read_header_timeout"`
	UpstreamResponseHeaderTimeout time.Duration `yaml:"upstream_response_header_timeout"`
}

type Provider struct {
	Upstream string `yaml:"upstream"`
	Format   string `yaml:"format"`
}

type Metering struct {
	RequireUser       bool     `yaml:"require_user"`
	InjectStreamUsage *bool    `yaml:"inject_stream_usage"`
	MetricTags        []string `yaml:"metric_tags"`
}

// InjectsStreamUsage reports whether include_usage injection is enabled (default true).
func (m Metering) InjectsStreamUsage() bool {
	return m.InjectStreamUsage == nil || *m.InjectStreamUsage
}

type Events struct {
	Output       string        `yaml:"output"`
	Dir          string        `yaml:"dir"`
	Filename     string        `yaml:"filename"`
	Rotate       time.Duration `yaml:"rotate"`
	OnWriteError string        `yaml:"on_write_error"`
}

// builtinProviders are always available and may be overridden in the config.
var builtinProviders = map[string]Provider{
	"openai":    {Upstream: "https://api.openai.com", Format: FormatOpenAI},
	"anthropic": {Upstream: "https://api.anthropic.com", Format: FormatAnthropic},
	"gemini":    {Upstream: "https://generativelanguage.googleapis.com", Format: FormatGemini},
	"xai":       {Upstream: "https://api.x.ai", Format: FormatOpenAI},
}

var (
	envRef       = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	tagKey       = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	reserved     = map[string]bool{"metrics": true, "healthz": true}
)

// Load reads the file at path. An empty path yields the defaults.
func Load(path string) (*Config, error) {
	var raw []byte
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		raw = envRef.ReplaceAllFunc(b, func(m []byte) []byte {
			return []byte(os.Getenv(string(envRef.FindSubmatch(m)[1])))
		})
	}
	return Parse(raw)
}

// Parse decodes YAML (already env-expanded), applies defaults and validates.
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && err.Error() != "EOF" {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	s := &c.Server
	if s.Listen == "" {
		s.Listen = ":8655"
	}
	if s.InstanceID == "" {
		s.InstanceID, _ = os.Hostname()
	}
	if s.MaxRequestBody == 0 {
		s.MaxRequestBody = 32 << 20
	}
	if s.ShutdownGrace == 0 {
		s.ShutdownGrace = 60 * time.Second
	}
	if s.ReadHeaderTimeout == 0 {
		s.ReadHeaderTimeout = 10 * time.Second
	}
	if s.UpstreamResponseHeaderTimeout == 0 {
		s.UpstreamResponseHeaderTimeout = 10 * time.Minute
	}

	merged := make(map[string]Provider, len(builtinProviders)+len(c.Providers))
	for name, p := range builtinProviders {
		merged[name] = p
	}
	for name, p := range c.Providers {
		base := merged[name]
		if p.Upstream == "" {
			p.Upstream = base.Upstream
		}
		if p.Format == "" {
			p.Format = base.Format
		}
		if p.Format == "" {
			p.Format = FormatOpenAI
		}
		merged[name] = p
	}
	c.Providers = merged

	e := &c.Events
	if e.Output == "" {
		e.Output = OutputFile
	}
	if e.Dir == "" {
		e.Dir = "./events"
	}
	if e.Filename == "" {
		e.Filename = "events-{instance}-{time}.jsonl"
	}
	if e.Rotate == 0 {
		e.Rotate = time.Hour
	}
	if e.OnWriteError == "" {
		e.OnWriteError = FailOpen
	}
}

func (c *Config) validate() error {
	for _, name := range sortedKeys(c.Providers) {
		p := c.Providers[name]
		if !providerName.MatchString(name) || reserved[name] {
			return fmt.Errorf("providers: invalid name %q", name)
		}
		u, err := url.Parse(p.Upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("providers.%s: upstream must be an http(s) URL, got %q", name, p.Upstream)
		}
		switch p.Format {
		case FormatOpenAI, FormatAnthropic, FormatGemini:
		default:
			return fmt.Errorf("providers.%s: unknown format %q", name, p.Format)
		}
	}
	for _, k := range c.Metering.MetricTags {
		if !tagKey.MatchString(k) {
			return fmt.Errorf("metering.metric_tags: invalid tag key %q", k)
		}
	}
	switch c.Events.Output {
	case OutputFile, OutputStdout:
	default:
		return fmt.Errorf("events.output: must be file or stdout, got %q", c.Events.Output)
	}
	switch c.Events.OnWriteError {
	case FailOpen, FailClosed:
	default:
		return fmt.Errorf("events.on_write_error: must be fail_open or fail_closed, got %q", c.Events.OnWriteError)
	}
	if c.Events.Rotate < time.Minute {
		return fmt.Errorf("events.rotate: must be at least 1m")
	}
	if c.Server.MaxRequestBody < 0 {
		return fmt.Errorf("server.max_request_body: must be positive")
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ByteSize accepts plain byte counts or values like "10MiB", "512KiB", "1GiB".
type ByteSize int64

func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	v, err := ParseByteSize(node.Value)
	if err != nil {
		return err
	}
	*b = ByteSize(v)
	return nil
}

func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1}}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	return n * mult, nil
}
