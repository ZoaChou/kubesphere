// Copyright 2015 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package certmagic automates the obtaining and renewal of TLS certificates,
// including TLS & HTTPS best practices such as robust OCSP stapling, caching,
// HTTP->HTTPS redirects, and more.
//
// Its high-level API serves your HTTP handlers over HTTPS if you simply give
// the domain name(s) and the http.Handler; CertMagic will create and run
// the HTTPS server for you, fully managing certificates during the lifetime
// of the server. Similarly, it can be used to start TLS listeners or return
// a ready-to-use tls.Config -- whatever layer you need TLS for, CertMagic
// makes it easy. See the HTTPS, Listen, and TLS functions for that.
//
// If you need more control, create a Cache using NewCache() and then make
// a Config using New(). You can then call Manage() on the config. But if
// you use this lower-level API, you'll have to be sure to solve the HTTP
// and TLS-ALPN challenges yourself (unless you disabled them or use the
// DNS challenge) by using the provided Config.GetCertificate function
// in your tls.Config and/or Config.HTTPChallangeHandler in your HTTP
// handler.
//
// See the package's README for more instruction.
package certmagic

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-acme/lego/certcrypto"
)

// HTTPS serves mux for all domainNames using the HTTP
// and HTTPS ports, redirecting all HTTP requests to HTTPS.
// It uses the Default config.
//
// This high-level convenience function is opinionated and
// applies sane defaults for production use, including
// timeouts for HTTP requests and responses. To allow very
// long-lived connections, you should make your own
// http.Server values and use this package's Listen(), TLS(),
// or Config.TLSConfig() functions to customize to your needs.
// For example, servers which need to support large uploads or
// downloads with slow clients may need to use longer timeouts,
// thus this function is not suitable.
//
// Calling this function signifies your acceptance to
// the CA's Subscriber Agreement and/or Terms of Service.
func HTTPS(domainNames []string, mux http.Handler) error {
	if mux == nil {
		mux = http.DefaultServeMux
	}

	Default.Agreed = true
	cfg := NewDefault()

	err := cfg.Manage(domainNames)
	if err != nil {
		return err
	}

	httpWg.Add(1)
	defer httpWg.Done()

	// if we haven't made listeners yet, do so now,
	// and clean them up when all servers are done
	lnMu.Lock()
	if httpLn == nil && httpsLn == nil {
		httpLn, err = net.Listen("tcp", fmt.Sprintf(":%d", HTTPPort))
		if err != nil {
			lnMu.Unlock()
			return err
		}

		httpsLn, err = tls.Listen("tcp", fmt.Sprintf(":%d", HTTPSPort), cfg.TLSConfig())
		if err != nil {
			httpLn.Close()
			httpLn = nil
			lnMu.Unlock()
			return err
		}

		go func() {
			httpWg.Wait()
			lnMu.Lock()
			httpLn.Close()
			httpsLn.Close()
			lnMu.Unlock()
		}()
	}
	hln, hsln := httpLn, httpsLn
	lnMu.Unlock()

	// create HTTP/S servers that are configured
	// with sane default timeouts and appropriate
	// handlers (the HTTP server solves the HTTP
	// challenge and issues redirects to HTTPS,
	// while the HTTPS server simply serves the
	// user's handler)
	httpServer := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       5 * time.Second,
		Handler:           cfg.HTTPChallengeHandler(http.HandlerFunc(httpRedirectHandler)),
	}
	httpsServer := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       5 * time.Minute,
		Handler:           mux,
	}

	log.Printf("%v Serving HTTP->HTTPS on %s and %s",
		domainNames, hln.Addr(), hsln.Addr())

	go httpServer.Serve(hln)
	return httpsServer.Serve(hsln)
}

func httpRedirectHandler(w http.ResponseWriter, r *http.Request) {
	toURL := "https://"

	// since we redirect to the standard HTTPS port, we
	// do not need to include it in the redirect URL
	requestHost, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		requestHost = r.Host // host probably did not contain a port
	}

	toURL += requestHost
	toURL += r.URL.RequestURI()

	// get rid of this disgusting unencrypted HTTP connection 🤢
	w.Header().Set("Connection", "close")

	http.Redirect(w, r, toURL, http.StatusMovedPermanently)
}

// TLS enables management of certificates for domainNames
// and returns a valid tls.Config. It uses the Default
// config.
//
// Because this is a convenience function that returns
// only a tls.Config, it does not assume HTTP is being
// served on the HTTP port, so the HTTP challenge is
// disabled (no HTTPChallengeHandler is necessary). The
// package variable Default is modified so that the
// HTTP challenge is disabled.
//
// Calling this function signifies your acceptance to
// the CA's Subscriber Agreement and/or Terms of Service.
func TLS(domainNames []string) (*tls.Config, error) {
	Default.Agreed = true
	Default.DisableHTTPChallenge = true
	cfg := NewDefault()
	return cfg.TLSConfig(), cfg.Manage(domainNames)
}

// Listen manages certificates for domainName and returns a
// TLS listener. It uses the Default config.
//
// Because this convenience function returns only a TLS-enabled
// listener and does not presume HTTP is also being served,
// the HTTP challenge will be disabled. The package variable
// Default is modified so that the HTTP challenge is disabled.
//
// Calling this function signifies your acceptance to
// the CA's Subscriber Agreement and/or Terms of Service.
func Listen(domainNames []string) (net.Listener, error) {
	Default.Agreed = true
	Default.DisableHTTPChallenge = true
	cfg := NewDefault()
	err := cfg.Manage(domainNames)
	if err != nil {
		return nil, err
	}
	return tls.Listen("tcp", fmt.Sprintf(":%d", HTTPSPort), cfg.TLSConfig())
}

// Manage obtains certificates for domainNames and keeps them
// renewed using the Default config.
//
// This is a slightly lower-level function; you will need to
// wire up support for the ACME challenges yourself. You can
// obtain a Config to help you do that by calling NewDefault().
//
// You will need to ensure that you use a TLS config that gets
// certificates from this Config and that the HTTP and TLS-ALPN
// challenges can be solved. The easiest way to do this is to
// use NewDefault().TLSConfig() as your TLS config and to wrap
// your HTTP handler with NewDefault().HTTPChallengeHandler().
// If you don't have an HTTP server, you will need to disable
// the HTTP challenge.
//
// If you already have a TLS config you want to use, you can
// simply set its GetCertificate field to
// NewDefault().GetCertificate.
//
// Calling this function signifies your acceptance to
// the CA's Subscriber Agreement and/or Terms of Service.
func Manage(domainNames []string) error {
	Default.Agreed = true
	return NewDefault().Manage(domainNames)
}

// OnDemandConfig contains some state relevant for providing
// on-demand TLS. Important note: If you are using the
// MaxObtain property to limit the maximum number of certs
// to be issued, the count of how many certs were issued
// will be reset if this struct gets garbage-collected.
type OnDemandConfig struct {
	// If set, this function will be the absolute
	// authority on whether the hostname (according
	// to SNI) is allowed to try to get a cert.
	DecisionFunc func(name string) error

	// If no DecisionFunc is set, this whitelist
	// is the absolute authority as to whether
	// a certificate should be allowed to be tried.
	// Names are compared against SNI value.
	HostWhitelist []string

	// If no DecisionFunc or HostWhitelist are set,
	// then an HTTP request will be made to AskURL
	// to determine if a certificate should be
	// obtained. If the request fails or the response
	// is anything other than 2xx status code, the
	// issuance will be denied.
	AskURL *url.URL

	// If no DecisionFunc, HostWhitelist, or AskURL
	// are set, then only this many certificates may
	// be obtained on-demand; this field is required
	// if all others are empty, otherwise, all cert
	// issuances will fail.
	MaxObtain int32

	// The number of certificates that have been issued on-demand
	// by this config. It is only safe to modify this count atomically.
	// If it reaches MaxObtain, on-demand issuances must fail.
	// Note that this will necessarily be reset to 0 if the
	// struct leaves scope and/or gets garbage-collected.
	obtainedCount int32
}

// Allowed returns whether the issuance for name is allowed according to o.
func (o *OnDemandConfig) Allowed(name string) error {
	// The decision function has absolute authority, if set
	if o.DecisionFunc != nil {
		return o.DecisionFunc(name)
	}

	// Otherwise, the host whitelist has decision authority
	if len(o.HostWhitelist) > 0 {
		return o.checkWhitelistForObtainingNewCerts(name)
	}

	// Otherwise, a URL is checked for permission to issue this cert
	if o.AskURL != nil {
		return o.checkURLForObtainingNewCerts(name)
	}

	// Otherwise use the limit defined by the "max_certs" setting
	return o.checkLimitsForObtainingNewCerts(name)
}

func (o *OnDemandConfig) whitelistContains(name string) bool {
	for _, n := range o.HostWhitelist {
		if strings.ToLower(n) == strings.ToLower(name) {
			return true
		}
	}
	return false
}

func (o *OnDemandConfig) checkWhitelistForObtainingNewCerts(name string) error {
	if !o.whitelistContains(name) {
		return fmt.Errorf("%s: name is not whitelisted", name)
	}
	return nil
}

func (o *OnDemandConfig) checkURLForObtainingNewCerts(name string) error {
	client := http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("following http redirects is not allowed")
		},
	}

	// Copy the URL from the config in order to modify it for this request
	askURL := new(url.URL)
	*askURL = *o.AskURL

	query := askURL.Query()
	query.Set("domain", name)
	askURL.RawQuery = query.Encode()

	resp, err := client.Get(askURL.String())
	if err != nil {
		return fmt.Errorf("error checking %v to deterine if certificate for hostname '%s' should be allowed: %v", o.AskURL, name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("certificate for hostname '%s' not allowed, non-2xx status code %d returned from %v", name, resp.StatusCode, o.AskURL)
	}

	return nil
}

// checkLimitsForObtainingNewCerts checks to see if name can be issued right
// now according the maximum count defined in the configuration. If a non-nil
// error is returned, do not issue a new certificate for name.
func (o *OnDemandConfig) checkLimitsForObtainingNewCerts(name string) error {
	if o.MaxObtain == 0 {
		return fmt.Errorf("%s: no certificates allowed to be issued on-demand", name)
	}

	// User can set hard limit for number of certs for the process to issue
	if o.MaxObtain > 0 &&
		atomic.LoadInt32(&o.obtainedCount) >= o.MaxObtain {
		return fmt.Errorf("%s: maximum certificates issued (%d)", name, o.MaxObtain)
	}

	// Make sure name hasn't failed a challenge recently
	failedIssuanceMu.RLock()
	when, ok := failedIssuance[name]
	failedIssuanceMu.RUnlock()
	if ok {
		return fmt.Errorf("%s: throttled; refusing to issue cert since last attempt on %s failed", name, when.String())
	}

	// Make sure, if we've issued a few certificates already, that we haven't
	// issued any recently
	lastIssueTimeMu.Lock()
	since := time.Since(lastIssueTime)
	lastIssueTimeMu.Unlock()
	if atomic.LoadInt32(&o.obtainedCount) >= 10 && since < 10*time.Minute {
		return fmt.Errorf("%s: throttled; last certificate was obtained %v ago", name, since)
	}

	// Good to go 👍
	return nil
}

// failedIssuance is a set of names that we recently failed to get a
// certificate for from the ACME CA. They are removed after some time.
// When a name is in this map, do not issue a certificate for it on-demand.
var failedIssuance = make(map[string]time.Time)
var failedIssuanceMu sync.RWMutex

// lastIssueTime records when we last obtained a certificate successfully.
// If this value is recent, do not make any on-demand certificate requests.
var lastIssueTime time.Time
var lastIssueTimeMu sync.Mutex

// isLoopback returns true if the hostname of addr looks
// explicitly like a common local hostname. addr must only
// be a host or a host:port combination.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(strings.ToLower(addr))
	if err != nil {
		host = addr // happens if the addr is only a hostname
	}
	return host == "localhost" ||
		strings.Trim(host, "[]") == "::1" ||
		strings.HasPrefix(host, "127.")
}

// isInternal returns true if the IP of addr
// belongs to a private network IP range. addr
// must only be an IP or an IP:port combination.
// Loopback addresses are considered false.
func isInternal(addr string) bool {
	privateNetworks := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"fc00::/7",
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // happens if the addr is just a hostname, missing port
		// if we encounter an error, the brackets need to be stripped
		// because SplitHostPort didn't do it for us
		host = strings.Trim(host, "[]")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, privateNetwork := range privateNetworks {
		_, ipnet, _ := net.ParseCIDR(privateNetwork)
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// Default contains the package defaults for the
// various Config fields. This is used as a template
// when creating your own Configs with New(), and it
// is also used as the Config by all the high-level
// functions in this package.
//
// The fields of this value will be used for Config
// fields which are unset. Feel free to modify these
// defaults, but do not use this Config by itself: it
// is only a template. Valid configurations can be
// obtained by calling New() (if you have your own
// certificate cache) or NewDefault() (if you only
// need a single config and want to use the default
// cache). This is the only Config which can access
// the default certificate cache.
var Default = Config{
	CA:                           LetsEncryptProductionCA,
	RenewDurationBefore:          DefaultRenewDurationBefore,
	RenewDurationBeforeAtStartup: DefaultRenewDurationBeforeAtStartup,
	KeyType:                      certcrypto.EC256,
	Storage:                      defaultFileStorage,
}

const (
	// HTTPChallengePort is the officially-designated port for
	// the HTTP challenge according to the ACME spec.
	HTTPChallengePort = 80

	// TLSALPNChallengePort is the officially-designated port for
	// the TLS-ALPN challenge according to the ACME spec.
	TLSALPNChallengePort = 443
)

// Some well-known CA endpoints available to use.
const (
	LetsEncryptStagingCA    = "https://acme-staging-v02.api.letsencrypt.org/directory"
	LetsEncryptProductionCA = "https://acme-v02.api.letsencrypt.org/directory"
)

// Port variables must remain their defaults unless you
// forward packets from the defaults to whatever these
// are set to; otherwise ACME challenges will fail.
var (
	// HTTPPort is the port on which to serve HTTP
	// and, by extension, the HTTP challenge (unless
	// Default.AltHTTPPort is set).
	HTTPPort = 80

	// HTTPSPort is the port on which to serve HTTPS
	// and, by extension, the TLS-ALPN challenge
	// (unless Default.AltTLSALPNPort is set).
	HTTPSPort = 443
)

// Variables for conveniently serving HTTPS.
var (
	httpLn, httpsLn net.Listener
	lnMu            sync.Mutex
	httpWg          sync.WaitGroup
)
