package catalog

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Online image resolvability (BYO-image model, spore-host#392, libs#18).
//
// The shipped/global catalog must reference only PUBLIC images that any consumer
// can pull. This verifies that by anonymously HEAD-ing each image's manifest via
// the OCI registry v2 API, following the standard 401 Bearer-token challenge —
// no Docker daemon, no credentials. It's separate from the offline Validate():
// it touches the network, so callers run it deliberately (a CI gate), not on
// every load.

// defaultHTTPClient is the client used for registry probes; modest timeout so a
// hung registry can't stall CI indefinitely.
var resolveHTTPClient = &http.Client{Timeout: 20 * time.Second}

// bearerChallengeRe pulls realm/service/scope out of a WWW-Authenticate header.
var bearerParamRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// ResolvePublicImages checks that every container app in the given list has a
// PUBLIC, anonymously-pullable image:tag. It returns one error per app that is
// private (by visibility) or whose manifest is not anonymously resolvable. Apps
// without an image (definition-only or legacy launch_command) are skipped.
//
// Intended for a CI gate over the embedded catalog: ResolvePublicImages(List()).
func ResolvePublicImages(apps []AppEntry) []error {
	var errs []error
	for i := range apps {
		e := &apps[i]
		if !e.Containerized() {
			continue
		}
		if e.ImageVisibility() != VisibilityPublic {
			errs = append(errs, fmt.Errorf("%s: image %q is %s — the shipped catalog must be public (#392)", e.Name, e.Image, e.ImageVisibility()))
			continue
		}
		tag := e.TagDefault
		if tag == "" {
			errs = append(errs, fmt.Errorf("%s: no tag_default to resolve", e.Name))
			continue
		}
		if err := headManifest(e.Image, tag); err != nil {
			errs = append(errs, fmt.Errorf("%s: image %s:%s not anonymously pullable: %w", e.Name, e.Image, tag, err))
		}
	}
	return errs
}

// headManifest issues an anonymous HEAD for image:tag's manifest, following one
// Bearer-token challenge if the registry requires it. Returns nil iff the
// manifest exists and is anonymously accessible.
func headManifest(image, tag string) error {
	host, repo, err := splitRegistryRepo(image)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, tag)

	resp, status, err := doManifestHead(url, "")
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		// Standard OCI flow: fetch an anonymous token per the challenge, retry once.
		tok, terr := fetchAnonToken(resp)
		if terr != nil {
			return fmt.Errorf("auth challenge: %w", terr)
		}
		_, status, err = doManifestHead(url, tok)
		if err != nil {
			return err
		}
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("manifest not found (404) — wrong tag or repo?")
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("not anonymously accessible (%d) — image appears private", status)
	default:
		return fmt.Errorf("unexpected status %d", status)
	}
}

// doManifestHead performs the HEAD; returns the response (headers only), status,
// error. token is sent as a Bearer credential when non-empty.
func doManifestHead(url, token string) (*http.Response, int, error) {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return nil, 0, err
	}
	// Accept the common manifest media types (single + index/list).
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
	}, ", "))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := resolveHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, resp.StatusCode, nil
}

// fetchAnonToken parses the WWW-Authenticate Bearer challenge on resp and
// requests an anonymous token from the realm.
func fetchAnonToken(resp *http.Response) (string, error) {
	ch := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(strings.ToLower(ch), "bearer") {
		return "", fmt.Errorf("unexpected auth scheme %q", ch)
	}
	params := map[string]string{}
	for _, m := range bearerParamRe.FindAllStringSubmatch(ch, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("no realm in challenge %q", ch)
	}
	url := realm
	q := []string{}
	if s := params["service"]; s != "" {
		q = append(q, "service="+s)
	}
	if s := params["scope"]; s != "" {
		q = append(q, "scope="+s)
	}
	if len(q) > 0 {
		url += "?" + strings.Join(q, "&")
	}
	tr, err := resolveHTTPClient.Get(url) //nolint:noctx
	if err != nil {
		return "", err
	}
	defer tr.Body.Close()
	body, _ := io.ReadAll(tr.Body)
	if tr.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %d", tr.StatusCode)
	}
	// Token JSON uses "token" or "access_token"; grab whichever is present without
	// pulling in a struct (registries differ on which they send).
	tok := jsonStringField(body, "token")
	if tok == "" {
		tok = jsonStringField(body, "access_token")
	}
	if tok == "" {
		return "", fmt.Errorf("no token in registry auth response")
	}
	return tok, nil
}

// splitRegistryRepo splits an image ref into registry host and repository path.
// A ref without a dotted/colon'd first segment defaults to Docker Hub
// (registry-1.docker.io, library/ namespace), matching docker's own rules.
func splitRegistryRepo(image string) (host, repo string, err error) {
	if image == "" {
		return "", "", fmt.Errorf("empty image ref")
	}
	first := image
	rest := ""
	if i := strings.IndexByte(image, '/'); i >= 0 {
		first, rest = image[:i], image[i+1:]
	}
	// A registry host has a '.' or ':' (or is "localhost"); otherwise it's a Docker
	// Hub short ref like "ubuntu" or "myorg/app".
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		if rest == "" {
			return "", "", fmt.Errorf("image %q has a registry host but no repository", image)
		}
		return first, rest, nil
	}
	// Docker Hub.
	repo = image
	if !strings.Contains(image, "/") {
		repo = "library/" + image
	}
	return "registry-1.docker.io", repo, nil
}

// jsonStringField extracts a top-level string field value from JSON bytes
// without a full unmarshal — registries vary in token field name and we only
// need one string. Returns "" if absent.
func jsonStringField(b []byte, field string) string {
	key := `"` + field + `"`
	i := strings.Index(string(b), key)
	if i < 0 {
		return ""
	}
	s := string(b)[i+len(key):]
	c := strings.IndexByte(s, ':')
	if c < 0 {
		return ""
	}
	s = s[c+1:]
	q1 := strings.IndexByte(s, '"')
	if q1 < 0 {
		return ""
	}
	s = s[q1+1:]
	q2 := strings.IndexByte(s, '"')
	if q2 < 0 {
		return ""
	}
	return s[:q2]
}
