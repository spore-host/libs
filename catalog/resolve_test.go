package catalog

import "testing"

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
