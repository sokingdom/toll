package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":8080" || c.Server.MaxRequestBody != 32<<20 || c.Events.OnWriteError != FailOpen || c.Events.Output != OutputFile {
		t.Fatalf("unexpected defaults %+v", c)
	}
	for _, name := range []string{"openai", "anthropic", "gemini", "xai"} {
		if _, ok := c.Providers[name]; !ok {
			t.Fatalf("missing builtin provider %s", name)
		}
	}
	if !c.Metering.InjectsStreamUsage() {
		t.Fatal("injection must default to on")
	}
}

func TestLoad(t *testing.T) {
	t.Setenv("TOLL_TEST_TOKEN", "secret")
	path := filepath.Join(t.TempDir(), "toll.yaml")
	yaml := `
server:
  auth_token: ${TOLL_TEST_TOKEN}
  max_request_body: 10MiB
  shutdown_grace: 5s
providers:
  openai: { upstream: http://localhost:9999 }
  local-vllm: { upstream: http://vllm:8000 }
metering:
  inject_stream_usage: false
  metric_tags: [feature]
events:
  output: stdout
  on_write_error: fail_closed
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.AuthToken != "secret" || c.Server.MaxRequestBody != 10<<20 || c.Server.ShutdownGrace != 5*time.Second {
		t.Fatalf("server: %+v", c.Server)
	}
	if p := c.Providers["openai"]; p.Upstream != "http://localhost:9999" || p.Format != FormatOpenAI {
		t.Fatalf("openai: %+v", p)
	}
	if p := c.Providers["local-vllm"]; p.Format != FormatOpenAI {
		t.Fatalf("local-vllm: %+v", p)
	}
	if c.Metering.InjectsStreamUsage() || c.Events.OnWriteError != FailClosed {
		t.Fatalf("metering/events: %+v %+v", c.Metering, c.Events)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"reserved name":   "providers: { metrics: { upstream: http://x } }",
		"bad upstream":    "providers: { foo: { upstream: ftp://x } }",
		"bad format":      "providers: { foo: { upstream: http://x, format: soap } }",
		"bad output":      "events: { output: kafka }",
		"bad policy":      "events: { on_write_error: block }",
		"bad tag":         "metering: { metric_tags: [\"a-b\"] }",
		"unknown field":   "server: { listn: \":1\" }",
		"bad byte size":   "server: { max_request_body: lots }",
		"rotate too fast": "events: { rotate: 1s }",
	}
	for name, yaml := range cases {
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if !strings.HasPrefix(err.Error(), "config:") {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}
}
