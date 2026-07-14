package telemetry

import (
	"net/url"
	"os"
	"strings"
)

// Config configures [Setup]. The zero value is fully valid and disabled
// (Enabled: false) — see the package doc's Configuration section.
type Config struct {
	// Enabled gates the whole feature. It is gofer's own switch, never
	// derived from an environment variable — see [Config.withEnvDefaults].
	Enabled bool
	// Endpoint is the OTLP collector endpoint, e.g. "localhost:4317" or
	// "https://collector.example.com:4318". A bare host:port (no scheme) is
	// interpreted per Insecure (https when false, http when true).
	Endpoint string
	// Protocol selects the OTLP wire protocol: "grpc" (default) or "http"
	// (http/protobuf).
	Protocol string
	// ServiceName sets the resource's service.name attribute. Defaults to
	// "gofer".
	ServiceName string
	// Insecure disables TLS on the OTLP connection.
	Insecure bool
	// Headers are extra headers sent with every OTLP export request (e.g.
	// collector auth).
	Headers map[string]string
}

// withEnvDefaults returns a copy of c with any unset field filled from the
// standard OTel environment variables: OTEL_EXPORTER_OTLP_ENDPOINT,
// OTEL_EXPORTER_OTLP_PROTOCOL, OTEL_SERVICE_NAME, and
// OTEL_EXPORTER_OTLP_HEADERS. Enabled is deliberately NOT among them — an
// operator can point gofer at a collector via the environment without
// silently turning telemetry on; Enabled is gofer's own explicit gate (see
// [Config]).
func (c Config) withEnvDefaults() Config {
	if c.Endpoint == "" {
		c.Endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if c.Protocol == "" {
		c.Protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if c.ServiceName == "" {
		c.ServiceName = os.Getenv("OTEL_SERVICE_NAME")
	}
	if len(c.Headers) == 0 {
		if h := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); h != "" {
			c.Headers = parseHeaders(h)
		}
	}
	if c.Protocol == "" {
		c.Protocol = "grpc"
	}
	if c.ServiceName == "" {
		c.ServiceName = "gofer"
	}
	return c
}

// parseHeaders parses the OTel env convention for OTEL_EXPORTER_OTLP_HEADERS:
// comma-separated "key=value" pairs, values optionally percent-encoded. An
// unparsable pair is skipped rather than failing the whole list. Returns nil
// for an empty result.
func parseHeaders(s string) map[string]string {
	var out map[string]string
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		v = strings.TrimSpace(v)
		if dv, err := url.QueryUnescape(v); err == nil {
			v = dv
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[k] = v
	}
	return out
}

// isHTTPProtocol reports whether the configured protocol names the OTLP
// HTTP transport (any of the "http/..." OTel env values), as opposed to the
// default gRPC transport.
func isHTTPProtocol(protocol string) bool {
	return strings.HasPrefix(protocol, "http")
}

// endpointURL derives a scheme-qualified endpoint URL for the OTLP exporter
// option constructors (all of which accept a full URL and infer
// insecure-vs-TLS from its scheme). An endpoint that already carries a
// scheme is passed through unchanged; a bare host:port is qualified from
// cfg.Insecure.
func endpointURL(cfg Config) string {
	if cfg.Endpoint == "" || strings.Contains(cfg.Endpoint, "://") {
		return cfg.Endpoint
	}
	scheme := "https"
	if cfg.Insecure {
		scheme = "http"
	}
	return scheme + "://" + cfg.Endpoint
}
