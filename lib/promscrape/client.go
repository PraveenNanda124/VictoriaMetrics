package promscrape

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/proxy"
	"github.com/VictoriaMetrics/fasthttp"
	"github.com/VictoriaMetrics/metrics"
)

var (
	maxScrapeSize = flagutil.NewBytes("promscrape.maxScrapeSize", 16*1024*1024, "The maximum size of scrape response in bytes to process from Prometheus targets. "+
		"Bigger responses are rejected")
	maxResponseHeadersSize = flagutil.NewBytes("promscrape.maxResponseHeadersSize", 4096, "The maximum size of http response headers from Prometheus scrape targets")
	disableCompression     = flag.Bool("promscrape.disableCompression", false, "Whether to disable sending 'Accept-Encoding: gzip' request headers to all the scrape targets. "+
		"This may reduce CPU usage on scrape targets at the cost of higher network bandwidth utilization. "+
		"It is possible to set 'disable_compression: true' individually per each 'scrape_config' section in '-promscrape.config' for fine grained control")
	disableKeepAlive = flag.Bool("promscrape.disableKeepAlive", false, "Whether to disable HTTP keep-alive connections when scraping all the targets. "+
		"This may be useful when targets has no support for HTTP keep-alive connection. "+
		"It is possible to set 'disable_keepalive: true' individually per each 'scrape_config' section in '-promscrape.config' for fine grained control. "+
		"Note that disabling HTTP keep-alive may increase load on both vmagent and scrape targets")
	streamParse = flag.Bool("promscrape.streamParse", false, "Whether to enable stream parsing for metrics obtained from scrape targets. This may be useful "+
		"for reducing memory usage when millions of metrics are exposed per each scrape target. "+
		"It is posible to set 'stream_parse: true' individually per each 'scrape_config' section in '-promscrape.config' for fine grained control")
)

type client struct {
	// hc is the default client optimized for common case of scraping targets with moderate number of metrics.
	hc *fasthttp.HostClient

	// sc (aka `stream client`) is used instead of hc if ScrapeWork.ParseStream is set.
	// It may be useful for scraping targets with millions of metrics per target.
	sc *http.Client

	scrapeURL               string
	scrapeTimeoutSecondsStr string
	hostPort                string
	requestURI              string
	setHeaders              func(req *http.Request)
	setProxyHeaders         func(req *http.Request)
	setFasthttpHeaders      func(req *fasthttp.Request)
	setFasthttpProxyHeaders func(req *fasthttp.Request)
	denyRedirects           bool
	disableCompression      bool
	disableKeepAlive        bool
}

func addMissingPort(addr string, isTLS bool) string {
	if strings.Contains(addr, ":") {
		return addr
	}
	if isTLS {
		return concatTwoStrings(addr, ":443")
	}
	return concatTwoStrings(addr, ":80")
}

func concatTwoStrings(x, y string) string {
	bb := bbPool.Get()
	b := bb.B[:0]
	b = append(b, x...)
	b = append(b, y...)
	s := bytesutil.ToUnsafeString(b)
	s = bytesutil.InternString(s)
	bb.B = b
	bbPool.Put(bb)
	return s
}

func newClient(sw *ScrapeWork) *client {
	var u fasthttp.URI
	u.Update(sw.ScrapeURL)
	hostPort := string(u.Host())
	dialAddr := hostPort
	requestURI := string(u.RequestURI())
	isTLS := string(u.Scheme()) == "https"
	var tlsCfg *tls.Config
	if isTLS {
		tlsCfg = sw.AuthConfig.NewTLSConfig()
	}
	setProxyHeaders := func(req *http.Request) {}
	setFasthttpProxyHeaders := func(req *fasthttp.Request) {}
	proxyURL := sw.ProxyURL
	if !isTLS && proxyURL.IsHTTPOrHTTPS() {
		// Send full sw.ScrapeURL in requests to a proxy host for non-TLS scrape targets
		// like net/http package from Go does.
		// See https://en.wikipedia.org/wiki/Proxy_server#Web_proxy_servers
		pu := proxyURL.GetURL()
		dialAddr = pu.Host
		requestURI = sw.ScrapeURL
		isTLS = pu.Scheme == "https"
		if isTLS {
			tlsCfg = sw.ProxyAuthConfig.NewTLSConfig()
		}
		proxyURLOrig := proxyURL
		setProxyHeaders = func(req *http.Request) {
			proxyURLOrig.SetHeaders(sw.ProxyAuthConfig, req)
		}
		setFasthttpProxyHeaders = func(req *fasthttp.Request) {
			proxyURLOrig.SetFasthttpHeaders(sw.ProxyAuthConfig, req)
		}
		proxyURL = &proxy.URL{}
	}
	hostPort = addMissingPort(hostPort, isTLS)
	dialAddr = addMissingPort(dialAddr, isTLS)
	dialFunc, err := newStatDialFunc(proxyURL, sw.ProxyAuthConfig)
	if err != nil {
		logger.Fatalf("cannot create dial func: %s", err)
	}
	hc := &fasthttp.HostClient{
		Addr:                         dialAddr,
		Name:                         "vm_promscrape",
		Dial:                         dialFunc,
		IsTLS:                        isTLS,
		TLSConfig:                    tlsCfg,
		MaxIdleConnDuration:          2 * sw.ScrapeInterval,
		ReadTimeout:                  sw.ScrapeTimeout,
		WriteTimeout:                 10 * time.Second,
		MaxResponseBodySize:          maxScrapeSize.N,
		MaxIdempotentRequestAttempts: 1,
		ReadBufferSize:               maxResponseHeadersSize.N,
	}
	var sc *http.Client
	var proxyURLFunc func(*http.Request) (*url.URL, error)
	if pu := sw.ProxyURL.GetURL(); pu != nil {
		proxyURLFunc = http.ProxyURL(pu)
	}
	sc = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:        tlsCfg,
			Proxy:                  proxyURLFunc,
			TLSHandshakeTimeout:    10 * time.Second,
			IdleConnTimeout:        2 * sw.ScrapeInterval,
			DisableCompression:     *disableCompression || sw.DisableCompression,
			DisableKeepAlives:      *disableKeepAlive || sw.DisableKeepAlive,
			DialContext:            statStdDial,
			MaxIdleConnsPerHost:    100,
			MaxResponseHeaderBytes: int64(maxResponseHeadersSize.N),

			// Set timeout for receiving the first response byte,
			// since the duration for reading the full response can be much bigger because of stream parsing.
			// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/1017#issuecomment-767235047
			ResponseHeaderTimeout: sw.ScrapeTimeout,
		},

		// Set 30x bigger timeout than the sw.ScrapeTimeout, since the duration for reading the full response
		// can be much bigger because of stream parsing.
		// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/1017#issuecomment-767235047
		Timeout: 30 * sw.ScrapeTimeout,
	}
	if sw.DenyRedirects {
		sc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return &client{
		hc:                      hc,
		sc:                      sc,
		scrapeURL:               sw.ScrapeURL,
		scrapeTimeoutSecondsStr: fmt.Sprintf("%.3f", sw.ScrapeTimeout.Seconds()),
		hostPort:                hostPort,
		requestURI:              requestURI,
		setHeaders:              func(req *http.Request) { sw.AuthConfig.SetHeaders(req, true) },
		setProxyHeaders:         setProxyHeaders,
		setFasthttpHeaders:      func(req *fasthttp.Request) { sw.AuthConfig.SetFasthttpHeaders(req, true) },
		setFasthttpProxyHeaders: setFasthttpProxyHeaders,
		denyRedirects:           sw.DenyRedirects,
		disableCompression:      sw.DisableCompression,
		disableKeepAlive:        sw.DisableKeepAlive,
	}
}

func (c *client) GetStreamReader() (*streamReader, error) {
	deadline := time.Now().Add(c.sc.Timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	req, err := http.NewRequestWithContext(ctx, "GET", c.scrapeURL, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cannot create request for %q: %w", c.scrapeURL, err)
	}
	// The following `Accept` header has been copied from Prometheus sources.
	// See https://github.com/prometheus/prometheus/blob/f9d21f10ecd2a343a381044f131ea4e46381ce09/scrape/scrape.go#L532 .
	// This is needed as a workaround for scraping stupid Java-based servers such as Spring Boot.
	// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/608 for details.
	// Do not bloat the `Accept` header with OpenMetrics shit, since it looks like dead standard now.
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=1,*/*;q=0.1")
	// Set X-Prometheus-Scrape-Timeout-Seconds like Prometheus does, since it is used by some exporters such as PushProx.
	// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/1179#issuecomment-813117162
	req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", c.scrapeTimeoutSecondsStr)
	c.setHeaders(req)
	c.setProxyHeaders(req)
	scrapeRequests.Inc()
	resp, err := c.sc.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cannot scrape %q: %w", c.scrapeURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		metrics.GetOrCreateCounter(fmt.Sprintf(`vm_promscrape_scrapes_total{status_code="%d"}`, resp.StatusCode)).Inc()
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("unexpected status code returned when scraping %q: %d; expecting %d; response body: %q",
			c.scrapeURL, resp.StatusCode, http.StatusOK, respBody)
	}
	scrapesOK.Inc()
	return &streamReader{
		r:           resp.Body,
		cancel:      cancel,
		scrapeURL:   c.scrapeURL,
		maxBodySize: int64(c.hc.MaxResponseBodySize),
	}, nil
}

// checks fasthttp status code for redirect as standard http/client does.
func isStatusRedirect(statusCode int) bool {
	switch statusCode {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

func (c *client) ReadData(dst []byte) ([]byte, error) {
	deadline := time.Now().Add(c.hc.ReadTimeout)
	req := fasthttp.AcquireRequest()
	req.SetRequestURI(c.requestURI)
	req.Header.SetHost(c.hostPort)
	// The following `Accept` header has been copied from Prometheus sources.
	// See https://github.com/prometheus/prometheus/blob/f9d21f10ecd2a343a381044f131ea4e46381ce09/scrape/scrape.go#L532 .
	// This is needed as a workaround for scraping stupid Java-based servers such as Spring Boot.
	// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/608 for details.
	// Do not bloat the `Accept` header with OpenMetrics shit, since it looks like dead standard now.
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=1,*/*;q=0.1")
	// Set X-Prometheus-Scrape-Timeout-Seconds like Prometheus does, since it is used by some exporters such as PushProx.
	// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/1179#issuecomment-813117162
	req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", c.scrapeTimeoutSecondsStr)
	c.setFasthttpHeaders(req)
	c.setFasthttpProxyHeaders(req)
	if !*disableCompression && !c.disableCompression {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	if *disableKeepAlive || c.disableKeepAlive {
		req.SetConnectionClose()
	}
	resp := fasthttp.AcquireResponse()
	swapResponseBodies := len(dst) == 0
	if swapResponseBodies {
		// An optimization: write response directly to dst.
		// This should reduce memory uage when scraping big targets.
		dst = resp.SwapBody(dst)
	}
	err := doRequestWithPossibleRetry(c.hc, req, resp, deadline)
	statusCode := resp.StatusCode()
	redirectsCount := 0
	for err == nil && isStatusRedirect(statusCode) {
		if redirectsCount > 5 {
			err = fmt.Errorf("too many redirects")
			break
		}
		if c.denyRedirects {
			err = fmt.Errorf("cannot follow redirects if `follow_redirects: false` is set")
			break
		}
		// It is expected that the redirect is made on the same host.
		// Otherwise it won't work.
		location := resp.Header.Peek("Location")
		if len(location) == 0 {
			err = fmt.Errorf("missing Location header")
			break
		}
		req.URI().UpdateBytes(location)
		err = doRequestWithPossibleRetry(c.hc, req, resp, deadline)
		statusCode = resp.StatusCode()
		redirectsCount++
	}
	if swapResponseBodies {
		dst = resp.SwapBody(dst)
	}
	fasthttp.ReleaseRequest(req)
	if err != nil {
		fasthttp.ReleaseResponse(resp)
		if err == fasthttp.ErrTimeout {
			scrapesTimedout.Inc()
			return dst, fmt.Errorf("error when scraping %q with timeout %s: %w", c.scrapeURL, c.hc.ReadTimeout, err)
		}
		if err == fasthttp.ErrBodyTooLarge {
			maxScrapeSizeExceeded.Inc()
			return dst, fmt.Errorf("the response from %q exceeds -promscrape.maxScrapeSize=%d; "+
				"either reduce the response size for the target or increase -promscrape.maxScrapeSize", c.scrapeURL, maxScrapeSize.N)
		}
		return dst, fmt.Errorf("error when scraping %q: %w", c.scrapeURL, err)
	}
	if ce := resp.Header.Peek("Content-Encoding"); string(ce) == "gzip" {
		var err error
		if swapResponseBodies {
			zb := gunzipBufPool.Get()
			zb.B, err = fasthttp.AppendGunzipBytes(zb.B[:0], dst)
			dst = append(dst[:0], zb.B...)
			gunzipBufPool.Put(zb)
		} else {
			dst, err = fasthttp.AppendGunzipBytes(dst, resp.Body())
		}
		if err != nil {
			fasthttp.ReleaseResponse(resp)
			scrapesGunzipFailed.Inc()
			return dst, fmt.Errorf("cannot ungzip response from %q: %w", c.scrapeURL, err)
		}
		scrapesGunzipped.Inc()
	} else if !swapResponseBodies {
		dst = append(dst, resp.Body()...)
	}
	fasthttp.ReleaseResponse(resp)
	if statusCode != fasthttp.StatusOK {
		metrics.GetOrCreateCounter(fmt.Sprintf(`vm_promscrape_scrapes_total{status_code="%d"}`, statusCode)).Inc()
		return dst, fmt.Errorf("unexpected status code returned when scraping %q: %d; expecting %d; response body: %q",
			c.scrapeURL, statusCode, fasthttp.StatusOK, dst)
	}
	scrapesOK.Inc()
	return dst, nil
}

var gunzipBufPool bytesutil.ByteBufferPool

var (
	maxScrapeSizeExceeded = metrics.NewCounter(`vm_promscrape_max_scrape_size_exceeded_errors_total`)
	scrapesTimedout       = metrics.NewCounter(`vm_promscrape_scrapes_timed_out_total`)
	scrapesOK             = metrics.NewCounter(`vm_promscrape_scrapes_total{status_code="200"}`)
	scrapesGunzipped      = metrics.NewCounter(`vm_promscrape_scrapes_gunziped_total`)
	scrapesGunzipFailed   = metrics.NewCounter(`vm_promscrape_scrapes_gunzip_failed_total`)
	scrapeRequests        = metrics.NewCounter(`vm_promscrape_scrape_requests_total`)
	scrapeRetries         = metrics.NewCounter(`vm_promscrape_scrape_retries_total`)
)

func doRequestWithPossibleRetry(hc *fasthttp.HostClient, req *fasthttp.Request, resp *fasthttp.Response, deadline time.Time) error {
	return discoveryutils.DoRequestWithPossibleRetry(hc, req, resp, deadline, scrapeRequests, scrapeRetries)
}

type streamReader struct {
	r           io.ReadCloser
	cancel      context.CancelFunc
	bytesRead   int64
	scrapeURL   string
	maxBodySize int64
}

func (sr *streamReader) Read(p []byte) (int, error) {
	n, err := sr.r.Read(p)
	sr.bytesRead += int64(n)
	if err == nil && sr.bytesRead > sr.maxBodySize {
		maxScrapeSizeExceeded.Inc()
		err = fmt.Errorf("the response from %q exceeds -promscrape.maxScrapeSize=%d; "+
			"either reduce the response size for the target or increase -promscrape.maxScrapeSize", sr.scrapeURL, sr.maxBodySize)
	}
	return n, err
}

func (sr *streamReader) MustClose() {
	sr.cancel()
	if err := sr.r.Close(); err != nil {
		logger.Errorf("cannot close reader: %s", err)
	}
}
