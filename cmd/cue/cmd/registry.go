package cmd

import (
	"log/slog"
	"net/http"
	"os"

	"cuelang.org/go/internal/cuedebug"
	"cuelang.org/go/internal/httplog"
	"cuelang.org/go/internal/mod/modload"
	"cuelang.org/go/mod/modconfig"
)

// getRegistryResolver returns an implementation of [modregistry.Resolver]
// that resolves to registries as specified in the configuration.
func getRegistryResolver() (*modconfig.Resolver, error) {
	return modconfig.NewResolver(newModConfig(""))
}

func getCachedRegistry() (modload.Registry, error) {
	return modconfig.NewRegistry(newModConfig(""))
}

func newModConfig(registry string) *modconfig.Config {
	return &modconfig.Config{
		Transport:   httpTransport(),
		ClientType:  "cmd/cue",
		CUERegistry: registry,
	}
}

func httpTransport() http.RoundTripper {
	cuedebug.Init()

	// Start from the default transport but do not negotiate
	// compression. When a request has no Accept-Encoding header,
	// net/http adds "gzip" itself and transparently decompresses the
	// response, leaving Content-Length unknown. The OCI client needs
	// that length to build a descriptor, so a proxy such as Cloudflare
	// that compresses manifests on request makes module fetches fail
	// with "invalid descriptor in response: unknown content length".
	// With DisableCompression set, no Accept-Encoding header is sent.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true

	if !cuedebug.Flags.HTTP {
		return transport
	}
	return httplog.Transport(&httplog.TransportConfig{
		Transport: transport,
		// It would be nice to use the default slog logger,
		// but that does a terrible job of printing structured
		// values, so use JSON output instead.
		Logger: httplog.SlogLogger{
			Logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		},
	})
}
