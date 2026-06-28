package catalog

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Pure-parser tests (always run; no network).

func TestSplitRegistryRepo(t *testing.T) {
	tests := []struct {
		image    string
		wantHost string
		wantRepo string
		wantErr  bool
	}{
		{"public.ecr.aws/f8g1e7l5/paraview", "public.ecr.aws", "f8g1e7l5/paraview", false},
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/x", "123456789012.dkr.ecr.us-east-1.amazonaws.com", "x", false},
		{"ghcr.io/org/app", "ghcr.io", "org/app", false},
		{"ubuntu", "registry-1.docker.io", "library/ubuntu", false},
		{"myorg/app", "registry-1.docker.io", "myorg/app", false},
		{"localhost:5000/app", "localhost:5000", "app", false},
		{"", "", "", true},
		{"public.ecr.aws", "", "", true}, // host but no repo
	}
	for _, tt := range tests {
		host, repo, err := splitRegistryRepo(tt.image)
		if (err != nil) != tt.wantErr {
			t.Errorf("splitRegistryRepo(%q) err=%v wantErr=%v", tt.image, err, tt.wantErr)
			continue
		}
		if err == nil && (host != tt.wantHost || repo != tt.wantRepo) {
			t.Errorf("splitRegistryRepo(%q) = (%q,%q), want (%q,%q)", tt.image, host, repo, tt.wantHost, tt.wantRepo)
		}
	}
}

func TestJSONStringField(t *testing.T) {
	tests := []struct {
		body  string
		field string
		want  string
	}{
		{`{"token":"abc123","expires_in":300}`, "token", "abc123"},
		{`{"access_token": "xyz" }`, "access_token", "xyz"},
		{`{"foo":"bar"}`, "token", ""},
		{`not json`, "token", ""},
	}
	for _, tt := range tests {
		if got := jsonStringField([]byte(tt.body), tt.field); got != tt.want {
			t.Errorf("jsonStringField(%q,%q) = %q, want %q", tt.body, tt.field, got, tt.want)
		}
	}
}

// fakeRegistry stands up an OCI-v2 registry that requires a Bearer token,
// exercising the full anonymous-token challenge → manifest-HEAD flow without a
// real registry. knownTags maps "<repo>:<tag>" to existence.
func fakeRegistry(t *testing.T, knownTags map[string]bool) (host string, cleanup func()) {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	// Token endpoint: hand out an anonymous token for any scope.
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"token":"anon-test-token"}`)
	})
	// Manifest endpoint: 401-with-challenge until a Bearer token is presented,
	// then 200/404 by knownTags.
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="fake",scope="repository:x:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// path: /v2/<repo...>/manifests/<tag>
		const pfx, mid = "/v2/", "/manifests/"
		p := r.URL.Path[len(pfx):]
		i := strings.LastIndex(p, mid)
		if i < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := p[:i] + ":" + p[i+len(mid):]
		if knownTags[key] {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv = httptest.NewServer(mux)

	// Point probes at the test server over http.
	oldScheme := resolveScheme
	resolveScheme = "http"
	host = strings.TrimPrefix(srv.URL, "http://") // host:port
	return host, func() {
		resolveScheme = oldScheme
		srv.Close()
	}
}

func TestResolvePublicImages_TokenFlow(t *testing.T) {
	host, cleanup := fakeRegistry(t, map[string]bool{"team/paraview:5.13.2": true})
	defer cleanup()

	apps := []AppEntry{
		// Public image that EXISTS → resolves through the token challenge.
		{Name: "ok", Image: host + "/team/paraview", TagDefault: "5.13.2"},
		// Public image, missing tag → 404 error.
		{Name: "badtag", Image: host + "/team/paraview", TagDefault: "9.9.9"},
		// Private (by inference) → rejected before any network call.
		{Name: "priv", Image: "123456789012.dkr.ecr.us-east-1.amazonaws.com/x", TagDefault: "1"},
		// Recipe-only / no image → skipped (no error).
		{Name: "recipe", Recipe: "infra/x"},
	}
	errs := ResolvePublicImages(apps)
	if len(errs) != 2 {
		t.Fatalf("want 2 errors (badtag, priv), got %d: %v", len(errs), errs)
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "badtag") || !strings.Contains(joined, "not found") {
		t.Errorf("expected a 404 for badtag, got: %s", joined)
	}
	if !strings.Contains(joined, "priv") || !strings.Contains(joined, "private") {
		t.Errorf("expected private rejection, got: %s", joined)
	}
}
