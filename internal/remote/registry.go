package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	maxManifestBytes = 1 << 20
	maxTokenBytes    = 64 << 10
	// tokenTTL is shorter than the usual 5-minute registry token.
	tokenTTL = 4 * time.Minute

	dockerHub = "registry-1.docker.io"

	// SourceLabel is the OCI annotation naming an image's source code.
	SourceLabel = "org.opencontainers.image.source"
)

var (
	challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)
	// imagePattern is an image name without tag or digest.
	imagePattern = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]{1,5})?(/[a-z0-9]+([._-]+[a-z0-9]+)*)*$`)
	tagPattern   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// manifestTypes are the image manifests and indexes Image accepts.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ",")

// RegistryGet reads https://host/v2/<repository>/<rest> anonymously: it
// answers a bearer-token challenge with an anonymous token, kept for the
// next requests to the same repository.
func (c *Client) RegistryGet(ctx context.Context, host, repository, rest, accept string, limit int64) (*Response, error) {
	key := host + "/" + repository
	c.mu.Lock()
	tok := c.tokens[key]
	c.mu.Unlock()
	if c.now().After(tok.expires) {
		tok = cachedToken{}
	}
	req := Request{URL: "https://" + host + "/v2/" + repository + "/" + rest, Accept: accept, Token: tok.token, Limit: limit}
	resp, err := c.Get(ctx, req)
	var challenge *ChallengeError
	if !errors.As(err, &challenge) || tok.token != "" {
		return resp, err
	}
	if req.Token, err = c.anonymousToken(ctx, challenge.Challenge); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.tokens[key] = cachedToken{token: req.Token, expires: c.now().Add(tokenTTL)}
	c.mu.Unlock()
	return c.Get(ctx, req)
}

func (c *Client) anonymousToken(ctx context.Context, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("the registry requires credentials")
	}
	values := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(params, -1) {
		values[strings.ToLower(m[1])] = m[2]
	}
	realm, err := url.Parse(values["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", fmt.Errorf("unsupported token realm %q", values["realm"])
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if v := values[k]; v != "" {
			q.Set(k, v)
		}
	}
	realm.RawQuery = q.Encode()
	resp, err := c.Get(ctx, Request{URL: realm.String(), Limit: maxTokenBytes})
	if err != nil {
		return "", fmt.Errorf("anonymous registry token: %w", err)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(resp.Body, &tok); err != nil {
		return "", fmt.Errorf("anonymous registry token: %w", err)
	}
	if tok.Token == "" {
		tok.Token = tok.AccessToken
	}
	if tok.Token == "" {
		return "", errors.New("anonymous registry token: empty")
	}
	return tok.Token, nil
}

// ParseImage splits an image name, as written in a manifest without its
// tag ("ghcr.io/gethomepage/homepage", "jhaals/yopass", "nginx"), into its
// registry host and repository. Docker Hub names get their implicit host
// and "library/" namespace.
func ParseImage(image string) (string, string, error) {
	if !imagePattern.MatchString(image) {
		return "", "", fmt.Errorf("invalid image name %q: want registry/name, without tag or digest", image)
	}
	first, rest, ok := strings.Cut(image, "/")
	if !ok {
		return dockerHub, "library/" + image, nil
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return dockerHub, image, nil
	}
	if first == "docker.io" || first == "index.docker.io" {
		if !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
		return dockerHub, rest, nil
	}
	return first, rest, nil
}

// ValidTag reports whether tag can be an image tag.
func ValidTag(tag string) bool { return tagPattern.MatchString(tag) }

// ImageLabels returns the labels of an image at a tag, from the
// configuration of its linux/amd64 variant (or its only one).
func (c *Client) ImageLabels(ctx context.Context, image, tag string) (map[string]string, error) {
	host, repository, err := ParseImage(image)
	if err != nil {
		return nil, err
	}
	if !ValidTag(tag) {
		return nil, fmt.Errorf("invalid image tag %q", tag)
	}
	resp, err := c.RegistryGet(ctx, host, repository, "manifests/"+tag, manifestTypes, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("image %s:%s: %w", image, tag, err)
	}
	var manifest struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(resp.Body, &manifest); err != nil {
		return nil, fmt.Errorf("image %s:%s: invalid manifest: %w", image, tag, err)
	}
	if len(manifest.Manifests) > 0 {
		digest := manifest.Manifests[0].Digest
		for _, m := range manifest.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
				digest = m.Digest
				break
			}
		}
		if !validDigest(digest) {
			return nil, fmt.Errorf("image %s:%s: invalid manifest digest %q", image, tag, digest)
		}
		if resp, err = c.RegistryGet(ctx, host, repository, "manifests/"+digest, manifestTypes, maxManifestBytes); err != nil {
			return nil, fmt.Errorf("image %s:%s: %w", image, tag, err)
		}
		if err := json.Unmarshal(resp.Body, &manifest); err != nil {
			return nil, fmt.Errorf("image %s:%s: invalid manifest: %w", image, tag, err)
		}
	}
	if !validDigest(manifest.Config.Digest) {
		return nil, fmt.Errorf("image %s:%s: no image configuration", image, tag)
	}
	resp, err = c.RegistryGet(ctx, host, repository, "blobs/"+manifest.Config.Digest, "", maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("image %s:%s: %w", image, tag, err)
	}
	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(resp.Body, &config); err != nil {
		return nil, fmt.Errorf("image %s:%s: invalid configuration: %w", image, tag, err)
	}
	return config.Config.Labels, nil
}

// validDigest accepts sha256 digests only, so a digest is safe in a URL.
func validDigest(d string) bool {
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hexPart) != 64 {
		return false
	}
	return strings.Trim(hexPart, "0123456789abcdef") == ""
}

// GitHubRepo returns the owner and name of a GitHub repository URL, as an
// image's source label gives it ("https://github.com/o/r", "git@…", ".git").
func GitHubRepo(source string) (string, string, bool) {
	s := strings.TrimSuffix(strings.TrimSpace(source), ".git")
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "github.com/"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			parts := strings.Split(strings.Trim(rest, "/"), "/")
			if len(parts) >= 2 && githubName.MatchString(parts[0]) && githubName.MatchString(parts[1]) {
				return parts[0], parts[1], true
			}
		}
	}
	return "", "", false
}

var githubName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
