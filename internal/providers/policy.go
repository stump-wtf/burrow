// Provider-row allowlists (ADR-0005): what a row may name, checked twice.
//
// A row is two choices burrow acts on with its own privileges: the
// environment variable whose value becomes that row's bearer token, and the
// host that token is sent to. Written the other way round, a provider write
// is "read environment variable X and send its value to host Y", which is
// #36: any llm-admin token (or a forged console POST) could name a variable
// outside burrow's own BURROW_ namespace and any http(s) host, and the value
// left on the next member poll. A denylist on one of the two leaves the
// other, so both are allowlists, enforced where a row is written and again
// where burrow uses it, so a row written before the allowlist existed is
// covered too.
package providers

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrNotAllowed marks a refusal by an allowlist, as opposed to a row that is
// malformed. A write that carries either is refused; a row that only trips
// the allowlist can still be seeded and corrected, so the seed path needs to
// tell the two apart (ADR-0005: "Seeding").
var ErrNotAllowed = errors.New("outside the provider allowlist")

// The two configuration variables, named in every refusal: whoever reads the
// refusal has to know which list to change.
const (
	envPrefixConfigVar = "BURROW_PROVIDER_API_KEY_ENV_PREFIXES"
	baseURLHostConfVar = "BURROW_PROVIDER_BASE_URL_HOSTS"
)

// Policy is the pair of allowlists a Runtime and the write gate apply. Its
// zero value checks nothing: build one with NewPolicy.
type Policy struct {
	enforce     bool
	envPrefixes []string
	hosts       []string
}

// NewPolicy builds the enforcing policy from the two configuration lists
// (config.Config.KeyEnvPrefixes and config.Config.GatewayHosts). An empty
// list allows nothing on its dimension: that is the point of a fail-closed
// host list, and the reason the prefix list has a default in config instead.
func NewPolicy(envPrefixes, hosts []string) Policy {
	p := Policy{enforce: true}
	for _, prefix := range envPrefixes {
		if prefix = strings.ToUpper(strings.TrimSpace(prefix)); prefix != "" {
			p.envPrefixes = append(p.envPrefixes, prefix)
		}
	}
	for _, host := range hosts {
		if host = normalizeHostEntry(host); host != "" {
			p.hosts = append(p.hosts, host)
		}
	}
	return p
}

// Unconstrained is the policy that checks nothing. Production always builds
// one from the config; this exists for a caller with no provider surface to
// police, which in practice means a test.
func Unconstrained() Policy { return Policy{} }

// CheckAPIKeyEnv reports whether a row may name that environment variable.
func (p Policy) CheckAPIKeyEnv(apiKeyEnv string) error {
	if !p.enforce {
		return nil
	}
	name := strings.ToUpper(strings.TrimSpace(apiKeyEnv))
	for _, prefix := range p.envPrefixes {
		if strings.HasPrefix(name, prefix) {
			return nil
		}
	}
	return fmt.Errorf("apiKeyEnv %q does not start with an allowed prefix (%s); name a provider key variable or add its prefix to %s: %w",
		apiKeyEnv, listOrNone(p.envPrefixes), envPrefixConfigVar, ErrNotAllowed)
}

// CheckBaseURL reports whether a row may point at that URL. It restates the
// shape checks a write already passed, because the rows this guards were
// written before them: only an origin with no credentials and no query is an
// origin burrow will send a key to.
func (p Policy) CheckBaseURL(baseURL string) error {
	if !p.enforce {
		return nil
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("baseURL %q must be an absolute http(s) URL: %w", baseURL, ErrNotAllowed)
	}
	if u.User != nil {
		return fmt.Errorf("baseURL must not carry credentials (user:password@host): %w", ErrNotAllowed)
	}
	if strings.ContainsAny(baseURL, "?#") {
		return fmt.Errorf("baseURL %q must not carry a query string or fragment: %w", baseURL, ErrNotAllowed)
	}
	host := requestHost(u)
	for _, entry := range p.hosts {
		if host == entry {
			return nil
		}
	}
	return fmt.Errorf("baseURL host %q is not on the gateway host allowlist (%s); add it to %s: %w",
		host, listOrNone(p.hosts), baseURLHostConfVar, ErrNotAllowed)
}

// CheckRow is both checks, in the order that reads best in a refusal: the
// key variable first, since a row that cannot name one serves nothing
// whatever its host.
func (p Policy) CheckRow(apiKeyEnv, baseURL string) error {
	if err := p.CheckAPIKeyEnv(apiKeyEnv); err != nil {
		return err
	}
	return p.CheckBaseURL(baseURL)
}

// normalizeHostEntry is a configured host as it is compared: lowercase, with
// any scheme or path a person typed dropped, and a default port left off so
// "litellm.example:443" and "litellm.example" are one entry.
func normalizeHostEntry(entry string) string {
	entry = strings.TrimSpace(entry)
	if i := strings.Index(entry, "://"); i >= 0 {
		u, err := url.Parse(entry)
		if err != nil {
			return ""
		}
		entry = u.Host
	}
	entry = strings.ToLower(entry)
	if i := strings.LastIndex(entry, ":"); i >= 0 {
		if port := entry[i+1:]; port == "80" || port == "443" {
			entry = entry[:i]
		}
	}
	return entry
}

// requestHost is the host of a row's URL in the same shape: lowercase, and
// with the scheme's default port left off. A non-default port stays part of
// the name, so an entry for a host does not authorize another service
// listening on it.
func requestHost(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	switch port := u.Port(); {
	case port == "":
	case port == "443" && u.Scheme == "https":
	case port == "80" && u.Scheme == "http":
	default:
		host += ":" + port
	}
	return host
}

// listOrNone spells an empty allowlist as nothing configured, so a refusal
// says which of the two mistakes it is.
func listOrNone(list []string) string {
	if len(list) == 0 {
		return "none configured"
	}
	return strings.Join(list, ", ")
}
