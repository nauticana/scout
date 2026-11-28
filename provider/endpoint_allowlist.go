package provider

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/domain"
)

// endpointAllowlist is where a registered endpoint may be dialed: host names, which reach public
// addresses only, and IP prefixes, the only way to reach a private address.
type endpointAllowlist struct {
	hosts    map[string]bool
	prefixes []netip.Prefix
}

func newEndpointAllowlist(entries []string) (endpointAllowlist, error) {
	allowlist := endpointAllowlist{hosts: map[string]bool{}}
	for _, entry := range entries {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			allowlist.prefixes = append(allowlist.prefixes, prefix.Masked())
		} else if addr, err := netip.ParseAddr(entry); err == nil {
			allowlist.prefixes = append(allowlist.prefixes, netip.PrefixFrom(addr, addr.BitLen()))
		} else if strings.ContainsAny(entry, "/:") {
			return endpointAllowlist{}, fmt.Errorf("%w: model endpoint allowlist entry %q", domain.ErrValidation, entry)
		} else {
			allowlist.hosts[entry] = true
		}
	}
	return allowlist, nil
}

func (allowlist endpointAllowlist) listed(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range allowlist.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// client validates the endpoint URL and returns a client that re-checks every dialed address,
// so a listed name that resolves to an unlisted private address is refused; it uses no proxy
// and follows no redirect.
func (allowlist endpointAllowlist) client(rawURL string, transport *endpointTransport) (*http.Client, error) {
	endpoint, err := url.Parse(rawURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("%w: model endpoint URL must be http or https without credentials or query", domain.ErrValidation)
	}
	host := strings.ToLower(endpoint.Hostname())
	addr, isAddr := netip.ParseAddr(host)
	if !allowlist.hosts[host] && !(isAddr == nil && allowlist.listed(addr)) {
		return nil, fmt.Errorf("%w: model endpoint host %q is not in the allowlist", domain.ErrForbidden, host)
	}
	byName := isAddr != nil
	dialer := &net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		target, err := netip.ParseAddrPort(address)
		if err == nil && (allowlist.listed(target.Addr()) || (byName && common.IsPublicAddr(target.Addr()))) {
			return nil
		}
		return fmt.Errorf("%w: model endpoint address %s is not in the allowlist", domain.ErrForbidden, address)
	}}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DialContext = dialer.DialContext
	transport.base = base
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("%w: model endpoint redirects are refused", domain.ErrForbidden)
	}}, nil
}
