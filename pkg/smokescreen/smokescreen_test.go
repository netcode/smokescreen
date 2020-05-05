// +build !nounit

package smokescreen

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/smokescreen/pkg/smokescreen/conntrack"
)

var allowRanges = []string{
	"8.8.9.0/24",
	"10.0.1.0/24",
	"172.16.1.0/24",
	"192.168.1.0/24",
	"127.0.1.0/24",
}
var allowAddresses = []string{
	"10.0.0.1:321",
}
var denyRanges = []string{
	"1.1.1.1/32",
}
var denyAddresses = []string{
	"8.8.8.8:321",
}

type testCase struct {
	ip       string
	port     int
	expected ipType
}

func TestClassifyAddr(t *testing.T) {
	a := assert.New(t)

	conf := NewConfig()
	a.NoError(conf.SetDenyRanges(denyRanges))
	a.NoError(conf.SetDenyAddresses(denyAddresses))
	a.NoError(conf.SetAllowRanges(allowRanges))
	a.NoError(conf.SetAllowAddresses(allowAddresses))
	conf.ConnectTimeout = 10 * time.Second
	conf.ExitTimeout = 10 * time.Second
	conf.AdditionalErrorMessageOnDeny = "Proxy denied"

	testIPs := []testCase{
		testCase{"8.8.8.8", 1, ipAllowDefault},
		testCase{"8.8.9.8", 1, ipAllowUserConfigured},

		// Specific blocked networks
		testCase{"10.0.0.1", 1, ipDenyPrivateRange},
		testCase{"10.0.0.1", 321, ipAllowUserConfigured},
		testCase{"10.0.1.1", 1, ipAllowUserConfigured},
		testCase{"172.16.0.1", 1, ipDenyPrivateRange},
		testCase{"172.16.1.1", 1, ipAllowUserConfigured},
		testCase{"192.168.0.1", 1, ipDenyPrivateRange},
		testCase{"192.168.1.1", 1, ipAllowUserConfigured},
		testCase{"8.8.8.8", 321, ipDenyUserConfigured},
		testCase{"1.1.1.1", 1, ipDenyUserConfigured},

		// localhost
		testCase{"127.0.0.1", 1, ipDenyNotGlobalUnicast},
		testCase{"127.255.255.255", 1, ipDenyNotGlobalUnicast},
		testCase{"::1", 1, ipDenyNotGlobalUnicast},
		testCase{"127.0.1.1", 1, ipAllowUserConfigured},

		// ec2 metadata endpoint
		testCase{"169.254.169.254", 1, ipDenyNotGlobalUnicast},

		// Broadcast addresses
		testCase{"255.255.255.255", 1, ipDenyNotGlobalUnicast},
		testCase{"ff02:0:0:0:0:0:0:2", 1, ipDenyNotGlobalUnicast},
	}

	for _, test := range testIPs {
		localIP := net.ParseIP(test.ip)
		if localIP == nil {
			t.Errorf("Could not parse IP from string: %s", test.ip)
			continue
		}
		localAddr := net.TCPAddr{
			IP:   localIP,
			Port: test.port,
		}

		got := classifyAddr(conf, &localAddr)
		if got != test.expected {
			t.Errorf("Misclassified IP (%s): should be %s, but is instead %s.", localIP, test.expected, got)
		}
	}
}

func TestClearsErrorHeader(t *testing.T) {
	r := require.New(t)

	t.Run("Clears error header set by upstream", func(t *testing.T) {
		log.SetFlags(log.LstdFlags | log.Lshortfile)

		cfg, err := testConfig("test-trusted-srv")
		r.NoError(err)

		proxySrv := proxyServer(cfg)
		r.NoError(err)
		defer proxySrv.Close()

		// Create a http.Client that uses our proxy
		client, err := proxyClient(proxySrv.URL)
		r.NoError(err)

		// Talk "through" the proxy to our malicious upstream that sets the
		// error header.
		resp, err := client.Get("http://httpbin.org/response-headers?X-Smokescreen-Error=foobar&X-Smokescreen-Test=yes")
		r.NoError(err)

		// Should succeed
		if resp.StatusCode != 200 {
			t.Errorf("response had bad status: expected 200, got %d", resp.StatusCode)
		}

		// Verify the error header is not set.
		if h := resp.Header.Get(errorHeader); h != "" {
			t.Errorf("proxy did not strip %q header: %q", errorHeader, h)
		}

		// Verify we did get the other header, to confirm we're talking to the right thing
		if h := resp.Header.Get("X-Smokescreen-Test"); h != "yes" {
			t.Errorf("did not get expected header X-Smokescreen-Test: expected \"yes\", got %q", h)
		}
	})

	t.Run("Doesn't clear errors for allowed connections", func(t *testing.T) {
		cfg, err := testConfig("test-local-srv")
		r.NoError(err)

		// Immediately time out to simulate net.Dial timeouts
		cfg.ConnectTimeout = -1

		proxySrv := proxyServer(cfg)
		r.NoError(err)
		defer proxySrv.Close()

		// Create a http.Client that uses our proxy
		client, err := proxyClient(proxySrv.URL)
		r.NoError(err)

		resp, err := client.Get("http://127.0.1.1")
		r.NoError(err)

		// Verify the error header is still set
		h := resp.Header.Get(errorHeader)
		if h == "" {
			t.Errorf("proxy stripped %q header: %q", errorHeader, h)
		}
	})
}

func TestConsistentHostHeader(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)

	hostCh := make(chan string)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
		hostCh <- r.Host
	}))
	defer ts.Close()

	// Custom proxy config for the "remote" httptest.NewServer
	conf := NewConfig()
	conf.ConnTracker = conntrack.NewTracker(conf.IdleTimeout, nil, conf.Log, atomic.Value{})
	err := conf.SetAllowAddresses([]string{"127.0.0.1"})
	r.NoError(err)

	proxy := BuildProxy(conf)
	proxySrv := httptest.NewServer(proxy)

	client, err := proxyClient(proxySrv.URL)
	r.NoError(err)

	req, err := http.NewRequest("GET", ts.URL, nil)
	r.NoError(err)

	expectedHostHeader := req.Host
	go client.Do(req)

	select {
	case receivedHostHeader := <-hostCh:
		a.Equal(expectedHostHeader, receivedHostHeader)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for client request")
	}
}

func TestClearsTraceIDHeader(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)

	headerCh := make(chan string)
	respCh := make(chan bool)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
		headerCh <- r.Header.Get("X-Smokescreen-Trace-ID")
	}))
	defer ts.Close()

	// Custom proxy config for the "remote" httptest.NewServer
	var logHook logrustest.Hook
	conf := NewConfig()
	conf.Log.AddHook(&logHook)
	conf.ConnTracker = conntrack.NewTracker(conf.IdleTimeout, nil, conf.Log, atomic.Value{})
	err := conf.SetAllowAddresses([]string{"127.0.0.1"})
	r.NoError(err)

	proxy := BuildProxy(conf)
	proxySrv := httptest.NewServer(proxy)

	client, err := proxyClient(proxySrv.URL)
	r.NoError(err)

	req, err := http.NewRequest("GET", ts.URL, nil)
	r.NoError(err)
	req.Header.Set("X-Smokescreen-Trace-ID", "6c4aa514e3da13ef")

	go func() {
		client.Do(req)
		respCh <- true
	}()

	select {
	case receivedTraceIDCh := <-headerCh:
		a.Empty(receivedTraceIDCh)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for client request")
	}

	select {
	case <-respCh:
		entry := findCanonicalProxyDecision(logHook.AllEntries())
		r.NotNil(entry)
		a.NotEmpty(entry.Data["trace_id"])
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server response")
	}

}

func TestShuttingDownValue(t *testing.T) {
	a := assert.New(t)

	conf := NewConfig()
	conf.Port = 39381

	quit := make(chan interface{})
	go StartWithConfig(conf, quit)

	// These sleeps are not ideal, but there is a race with checking the
	// ShuttingDown value from these tests. The server has to bootstrap
	// itself with an initial value before it returns false, and has to
	// set the value to true after we send on the quit channel.
	time.Sleep(500 * time.Millisecond)
	a.Equal(false, conf.ShuttingDown.Load())

	quit <- true

	time.Sleep(500 * time.Millisecond)
	a.Equal(true, conf.ShuttingDown.Load())

}

func TestHealthcheck(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)

	healthcheckCh := make(chan string)

	testHealthcheck := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
		healthcheckCh <- "OK"
	})

	conf := NewConfig()

	// We set this here so that we can deterministically test the Healthcheck
	// handler. Otherwise we would have to call StartWithConfig() in a goroutine,
	// which creates a race between the test and the listener accepting
	// connections.
	handler := HealthcheckMiddleware{
		Proxy:       BuildProxy(conf),
		Healthcheck: testHealthcheck,
	}

	server := httptest.NewServer(handler)

	go func() {
		select {
		case healthy := <-healthcheckCh:
			a.Equal("OK", healthy)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for client request")
		}
	}()

	resp, err := http.Get(fmt.Sprintf("%s/healthcheck", server.URL))
	r.NoError(err)
	a.Equal(http.StatusOK, resp.StatusCode)
}

var invalidHostCases = []struct {
	scheme    string
	expectErr bool
	proxyType string
}{
	{"http", false, "http"},
	{"https", true, "connect"},
}

func TestInvalidHost(t *testing.T) {
	for _, testCase := range invalidHostCases {
		t.Run(testCase.scheme, func(t *testing.T) {
			a := assert.New(t)
			r := require.New(t)

			cfg, err := testConfig("test-trusted-srv")
			require.NoError(t, err)
			logHook := proxyLogHook(cfg)

			proxySrv := proxyServer(cfg)
			defer proxySrv.Close()

			// Create a http.Client that uses our proxy
			client, err := proxyClient(proxySrv.URL)
			r.NoError(err)

			resp, err := client.Get(fmt.Sprintf("%s://notarealhost.notarealtld", testCase.scheme))
			if testCase.expectErr {
				r.Contains(err.Error(), "Bad gateway")
			} else {
				r.NoError(err)
				r.Equal(http.StatusBadGateway, resp.StatusCode)
			}

			entry := findCanonicalProxyDecision(logHook.AllEntries())
			r.NotNil(entry)

			if a.Contains(entry.Data, "allow") {
				a.Equal(true, entry.Data["allow"])
			}
			if a.Contains(entry.Data, "error") {
				a.Contains(entry.Data["error"], "no such host")
			}
			if a.Contains(entry.Data, "proxy_type") {
				a.Contains(entry.Data["proxy_type"], testCase.proxyType)
			}
		})
	}
}

func TestErrorHeader(t *testing.T) {
	a := assert.New(t)
	r := require.New(t)

	cfg, err := testConfig("test-trusted-srv")
	require.NoError(t, err)
	logHook := proxyLogHook(cfg)

	proxySrv := proxyServer(cfg)
	defer proxySrv.Close()

	// Create a http.Client that uses our proxy
	client, err := proxyClient(proxySrv.URL)
	r.NoError(err)

	resp, err := client.Get("http://example.com")
	r.NoError(err)
	r.Equal(http.StatusProxyAuthRequired, resp.StatusCode)
	r.NotEmpty(resp.Header.Get("X-Smokescreen-Error"))

	entry := findCanonicalProxyDecision(logHook.AllEntries())
	r.NotNil(entry)

	if a.Contains(entry.Data, "allow") {
		a.Equal(false, entry.Data["allow"])
	}
}

// TestProxyProtocols ensures that both traditional HTTP and CONNECT proxy
// requests Emit the correct CANONICAL-PROXY-DECISION log
func TestProxyProtocols(t *testing.T) {
	a := assert.New(t)
	r := require.New(t)
	t.Run("HTTP proxy", func(t *testing.T) {
		cfg, err := testConfig("test-local-srv")
		r.NoError(err)
		err = cfg.SetAllowAddresses([]string{"127.0.0.1"})
		r.NoError(err)

		clientCh := make(chan bool)
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("OK"))
		})

		l, err := net.Listen("tcp", "localhost:0")
		r.NoError(err)
		cfg.Listener = l

		logHook := proxyLogHook(cfg)
		proxy := proxyServer(cfg)
		remote := httptest.NewServer(h)
		client, err := proxyClient(proxy.URL)
		r.NoError(err)

		req, err := http.NewRequest("GET", remote.URL, nil)
		r.NoError(err)

		go func() {
			client.Do(req)
			clientCh <- true
		}()

		<-clientCh
		entry := findCanonicalProxyDecision(logHook.AllEntries())
		r.NotNil(entry)

		r.Contains(entry.Data, "proxy_type")
		r.Equal("http", entry.Data["proxy_type"])
	})

	t.Run("CONNECT proxy", func(t *testing.T) {
		cfg, err := testConfig("test-local-srv")
		r.NoError(err)
		err = cfg.SetAllowAddresses([]string{"127.0.0.1"})
		r.NoError(err)

		clientCh := make(chan bool)
		serverCh := make(chan bool)
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serverCh <- true
			<-serverCh
			w.Write([]byte("OK"))
		})

		logHook := proxyLogHook(cfg)
		l, err := net.Listen("tcp", "localhost:0")
		r.NoError(err)
		cfg.Listener = l

		proxy := proxyServer(cfg)
		remote := httptest.NewTLSServer(h)
		client, err := proxyClient(proxy.URL)
		r.NoError(err)

		req, err := http.NewRequest("GET", remote.URL, nil)
		r.NoError(err)

		go func() {
			client.Do(req)
			clientCh <- true
		}()

		<-serverCh
		count := 0
		cfg.ConnTracker.Range(func(k, v interface{}) bool {
			count++
			return true
		})
		a.Equal(1, count, "connTracker should contain one tracked connection")

		serverCh <- true
		<-clientCh

		entry := findCanonicalProxyDecision(logHook.AllEntries())
		r.NotNil(entry)
		r.Contains(entry.Data, "proxy_type")
		r.Equal("connect", entry.Data["proxy_type"])
	})
}

func TestProxyTimeouts(t *testing.T) {
	r := require.New(t)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		w.Write([]byte("OK"))
	})

	t.Run("HTTP proxy timeouts", func(t *testing.T) {
		cfg, err := testConfig("test-local-srv")
		r.NoError(err)
		err = cfg.SetAllowAddresses([]string{"127.0.0.1"})
		r.NoError(err)

		logHook := proxyLogHook(cfg)
		cfg.IdleTimeout = 100 * time.Millisecond

		l, err := net.Listen("tcp", "localhost:0")
		r.NoError(err)
		cfg.Listener = l

		proxy := proxyServer(cfg)
		remote := httptest.NewServer(h)
		client, err := proxyClient(proxy.URL)
		r.NoError(err)

		req, err := http.NewRequest("GET", remote.URL, nil)
		r.NoError(err)

		resp, _ := client.Do(req)
		r.Equal(http.StatusGatewayTimeout, resp.StatusCode)
		r.NotEqual("", resp.Header.Get(errorHeader))

		entry := findCanonicalProxyDecision(logHook.AllEntries())
		r.NotNil(entry)

		r.Equal("http", entry.Data["proxy_type"])
		r.Contains(entry.Data["error"], "i/o timeout")
	})

	t.Run("CONNECT proxy timeouts", func(t *testing.T) {
		cfg, err := testConfig("test-local-srv")
		r.NoError(err)
		err = cfg.SetAllowAddresses([]string{"127.0.0.1"})
		r.NoError(err)

		logHook := proxyLogHook(cfg)
		cfg.IdleTimeout = 100 * time.Millisecond

		l, err := net.Listen("tcp", "localhost:0")
		r.NoError(err)
		cfg.Listener = l

		proxy := proxyServer(cfg)
		remote := httptest.NewTLSServer(h)
		client, err := proxyClient(proxy.URL)
		r.NoError(err)

		req, err := http.NewRequest("GET", remote.URL, nil)
		r.NoError(err)

		resp, err := client.Do(req)
		r.Nil(resp)
		r.Error(err)
		r.Contains(err.Error(), "EOF")

		entry := findCanonicalProxyClose(logHook.AllEntries())
		r.NotNil(entry)

		r.Equal(true, entry.Data["timed_out"])
		r.Contains(entry.Data["error"], "i/o timeout")
	})

	t.Run("CONNECT proxy dial timeouts", func(t *testing.T) {
		cfg, err := testConfig("test-local-srv")
		r.NoError(err)
		err = cfg.SetAllowAddresses([]string{"127.0.0.1"})
		r.NoError(err)

		cfg.ConnectTimeout = -1

		l, err := net.Listen("tcp", "localhost:0")
		r.NoError(err)
		cfg.Listener = l

		proxy := proxyServer(cfg)
		remote := httptest.NewTLSServer(h)
		client, err := proxyClient(proxy.URL)
		r.NoError(err)

		req, err := http.NewRequest("GET", remote.URL, nil)
		r.NoError(err)

		// Go swallows the response as the CONNECT tunnel was never established
		resp, err := client.Do(req)
		r.Nil(resp)
		r.Error(err)
		r.Contains(err.Error(), "Gateway timeout")
	})

	t.Run("HTTP proxy dial timeouts", func(t *testing.T) {
		cfg, err := testConfig("test-local-srv")
		r.NoError(err)
		err = cfg.SetAllowAddresses([]string{"127.0.0.1"})
		r.NoError(err)

		cfg.ConnectTimeout = -1

		l, err := net.Listen("tcp", "localhost:0")
		r.NoError(err)
		cfg.Listener = l

		proxy := proxyServer(cfg)
		remote := httptest.NewServer(h)
		client, err := proxyClient(proxy.URL)
		r.NoError(err)

		req, err := http.NewRequest("GET", remote.URL, nil)
		r.NoError(err)

		resp, _ := client.Do(req)
		r.Equal(http.StatusGatewayTimeout, resp.StatusCode)
		r.NotEqual("", resp.Header.Get(errorHeader))
	})
}

func findCanonicalProxyDecision(logs []*logrus.Entry) *logrus.Entry {
	for _, entry := range logs {
		if entry.Message == LOGLINE_CANONICAL_PROXY_DECISION {
			return entry
		}
	}
	return nil
}

func findCanonicalProxyClose(logs []*logrus.Entry) *logrus.Entry {
	for _, entry := range logs {
		if entry.Message == conntrack.CanonicalProxyConnClose {
			return entry
		}
	}
	return nil
}

func testConfig(role string) (*Config, error) {
	conf := NewConfig()

	if err := conf.SetAllowRanges(allowRanges); err != nil {
		return nil, err
	}
	conf.ConnectTimeout = 10 * time.Second
	conf.ExitTimeout = 10 * time.Second
	conf.AdditionalErrorMessageOnDeny = "Proxy denied"
	conf.Resolver = &net.Resolver{}
	conf.ConnTracker = conntrack.NewTracker(conf.IdleTimeout, nil, conf.Log, atomic.Value{})
	conf.SetupEgressAcl("testdata/acl.yaml")
	conf.RoleFromRequest = func(req *http.Request) (string, error) {
		return role, nil
	}
	return conf, nil
}

func proxyLogHook(conf *Config) *logrustest.Hook {
	var testHook logrustest.Hook
	conf.Log.AddHook(&testHook)
	return &testHook
}

func proxyServer(conf *Config) *httptest.Server {
	proxy := BuildProxy(conf)
	return httptest.NewServer(proxy)
}

func proxyClient(proxy string) (*http.Client, error) {
	proxyUrl, err := url.Parse(proxy)
	if err != nil {
		return nil, err
	}

	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyURL(proxyUrl),
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		},
	}, nil
}
