package telemetry

import (
	"reflect"
	"testing"
)

func TestConfig_WithEnvDefaults(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		env  map[string]string
		want Config
	}{
		{
			name: "zero value: enabled stays false, defaults fill in",
			cfg:  Config{},
			want: Config{Protocol: "grpc", ServiceName: "gofer"},
		},
		{
			name: "explicit fields are never overridden by env",
			cfg:  Config{Enabled: true, Endpoint: "explicit:4317", Protocol: "http", ServiceName: "svc"},
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "env:4317",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
				"OTEL_SERVICE_NAME":           "env-svc",
			},
			want: Config{Enabled: true, Endpoint: "explicit:4317", Protocol: "http", ServiceName: "svc"},
		},
		{
			name: "env fills unset endpoint/protocol/service_name; Enabled is NOT driven by env",
			cfg:  Config{},
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4317",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
				"OTEL_SERVICE_NAME":           "env-svc",
			},
			want: Config{Endpoint: "collector:4317", Protocol: "http/protobuf", ServiceName: "env-svc"},
		},
		{
			name: "env headers parsed",
			cfg:  Config{},
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_HEADERS": "api-key=secret-value,x-team=infra",
			},
			want: Config{
				Protocol:    "grpc",
				ServiceName: "gofer",
				Headers:     map[string]string{"api-key": "secret-value", "x-team": "infra"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := tc.cfg.withEnvDefaults()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("withEnvDefaults() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseHeaders(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{name: "empty", in: "", want: nil},
		{name: "single", in: "key=value", want: map[string]string{"key": "value"}},
		{
			name: "multiple, percent-encoded value",
			in:   "a=1,b=hello%20world",
			want: map[string]string{"a": "1", "b": "hello world"},
		},
		{name: "skips malformed pair", in: "a=1,noequals,b=2", want: map[string]string{"a": "1", "b": "2"}},
		{name: "skips empty key", in: "=novalue,a=1", want: map[string]string{"a": "1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseHeaders(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseHeaders(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsHTTPProtocol(t *testing.T) {
	tests := []struct {
		protocol string
		want     bool
	}{
		{"", false},
		{"grpc", false},
		{"http", true},
		{"http/protobuf", true},
		{"http/json", true},
	}
	for _, tc := range tests {
		if got := isHTTPProtocol(tc.protocol); got != tc.want {
			t.Errorf("isHTTPProtocol(%q) = %v, want %v", tc.protocol, got, tc.want)
		}
	}
}

func TestEndpointURL(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "empty endpoint stays empty", cfg: Config{}, want: ""},
		{name: "already scheme-qualified passes through", cfg: Config{Endpoint: "http://collector:4318"}, want: "http://collector:4318"},
		{name: "bare host:port defaults to https", cfg: Config{Endpoint: "collector:4317"}, want: "https://collector:4317"},
		{name: "bare host:port with Insecure uses http", cfg: Config{Endpoint: "collector:4317", Insecure: true}, want: "http://collector:4317"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := endpointURL(tc.cfg); got != tc.want {
				t.Errorf("endpointURL(%+v) = %q, want %q", tc.cfg, got, tc.want)
			}
		})
	}
}
