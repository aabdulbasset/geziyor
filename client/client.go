package client

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"

	"net/http"
	"net/url"
	"time"

	"crypto/rand"

	"github.com/andybalholm/brotli"
	cmap "github.com/orcaman/concurrent-map/v2"

	"github.com/aabdulbasset/geziyor/internal"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/gospider007/ja3"
	"github.com/gospider007/requests"
)

var (
	// ErrNoCookieJar is the error type for missing cookie jar
	ErrNoCookieJar = errors.New("cookie jar is not available")
)

// Client is a small wrapper around *http.Client to provide new methods.
type Client struct {
	*requests.Client
	opt       *Options
	Histogram cmap.ConcurrentMap[string, int]
}

// Options is custom http.client options
type Options struct {
	MaxBodySize           int64
	CharsetDetectDisabled bool
	RetryTimes            int
	RetryHTTPCodes        []int
	RemoteAllocatorURL    string
	AllocatorOptions      []chromedp.ExecAllocatorOption
	ProxyFunc             func(ctx *requests.Response) (string, error)
	Timeout               time.Duration
	CookiesDisabled       bool
	MaxRedirect           int
	// Changing this will override the existing default PreActions for Rendered requests.
	// Geziyor Response will be nearly empty. Because we have no way to extract response without default pre actions.
	// So, if you set this, you should handle all navigation, header setting, and response handling yourself.
	// See defaultPreActions variable for the existing defaults.
	PreActions        []chromedp.Action
	RequestMiddleware []ClientRequestMiddleware
}
type ClientRequestMiddleware interface {
	BeforeRequest(req *http.Request)
}

// Default values for client
const (
	DefaultUserAgent        = "Geziyor 2.0"
	DefaultMaxBody    int64 = 1024 * 1024 * 1024 // 1GB
	DefaultRetryTimes       = 2
)

var (
	DefaultRetryHTTPCodes = []int{500, 502, 503, 504, 522, 524, 408}
)

// NewClient creates http.Client with modified values for typical web scraper
func NewClient(opt *Options) *Client {

	// httpClient := &http.Client{
	// 	Transport: &http.Transport{
	// 		Proxy: proxyFunction,
	// 		DialContext: (&net.Dialer{
	// 			Timeout:   15 * time.Second,
	// 			KeepAlive: 15 * time.Second,
	// 			DualStack: true,
	// 		}).DialContext,
	// 		ForceAttemptHTTP2:     true,
	// 		MaxIdleConns:          0,    // Default: 100
	// 		MaxIdleConnsPerHost:   1000, // Default: 2
	// 		IdleConnTimeout:       15 * time.Second,
	// 		TLSHandshakeTimeout:   10 * time.Second,
	// 		ExpectContinueTimeout: 2 * time.Second,
	// 	},
	// 	Timeout: time.Second * 180, // Google's timeout
	// }
	options := requests.ClientOption{
		Timeout:             180 * time.Second,
		TlsHandshakeTimeout: 10 * time.Second,
		DialOption: requests.DialOption{
			DialTimeout: 15 * time.Second,
			KeepAlive:   15 * time.Second,
		},
	}
	if opt.Timeout != 0 {
		options.Timeout = opt.Timeout
	}
	if opt.CookiesDisabled {
		options.DisCookie = true
	}
	if opt.MaxRedirect != 0 {
		options.MaxRedirect = opt.MaxRedirect
	}

	httpClient, httpClientErr := requests.NewClient(context.Background(), options)
	if httpClientErr != nil {
		fmt.Println(httpClientErr)
		panic(httpClientErr)
	}

	client := Client{
		Client: httpClient,
		opt:    opt,
	}

	return &client
}

// DoRequest selects appropriate request handler, client or Chrome
func (c *Client) DoRequest(req *Request) (resp *Response, err error) {
	//assign id to request if none exists
	id := req.Meta["id"]
	if id == nil {
		b := make([]byte, 16)
		_, err := rand.Read(b)
		if err != nil {
			return nil, err
		}
		req.Meta["id"] = fmt.Sprintf("%x", b)
	}

	for _, middleware := range c.opt.RequestMiddleware {
		middleware.BeforeRequest(req.Request)
	}

	if req.Rendered {
		resp, err = c.doRequestChrome(req)
	} else {
		resp, err = c.doRequestClient(req)
	}

	// Retry on Error
	if err != nil {
		if req.retryCounter < c.opt.RetryTimes {
			req.retryCounter++
			internal.Logger.Println("Retrying:", req.URL.String())
			return c.DoRequest(req)
		}
		return resp, err
	}

	// Retry on http status codes
	if internal.ContainsInt(c.opt.RetryHTTPCodes, resp.StatusCode) {
		if req.retryCounter < c.opt.RetryTimes {
			req.retryCounter++
			internal.Logger.Println("Retrying:", req.URL.String(), resp.StatusCode)
			return c.DoRequest(req)
		}
	}
	c.Histogram.Set(req.Meta["id"].(string), req.retryCounter)
	return resp, err
}

// doRequestClient is a simple wrapper to read response according to options.
func (c *Client) doRequestClient(req *Request) (*Response, error) {

	reqOptions := requests.RequestOption{}
	if req.Header.Get("user-agent") != "" {
		userAgent := req.Header.Get("user-agent")
		reqOptions.UserAgent = userAgent
		if strings.Contains(strings.ToLower(userAgent), "firefox") {

			reqOptions.Ja3 = true
			reqOptions.H3 = true
			reqOptions.Headers = DefaultFirefoxHeaders()
			reqOptions.Ja3Spec, reqOptions.H2Ja3Spec = DefaultFirefoxSpec()
		}
		if strings.Contains(strings.ToLower(userAgent), "chrome") {
			reqOptions.OrderHeaders = []string{":method",
				":authority",
				":scheme",
				":path",
				"sec-ch-ua",
				"sec-ch-ua-mobile",
				"sec-ch-ua-platform",
				"upgrade-insecure-requests",
				"user-agent",
				"accept",
				"sec-gpc",
				"accept-language",
				"sec-fetch-site",
				"sec-fetch-mode",
				"sec-fetch-user",
				"sec-fetch-dest",
				"accept-encoding",
				"priority"}

			reqOptions.Ja3 = true
			reqOptions.H3 = true
			reqOptions.Headers = DefaultChromeHeaders()
			reqOptions.Ja3Spec, reqOptions.H2Ja3Spec = DefaultChromeSpec()
		}

	}
	if c.opt.ProxyFunc != nil {
		reqOptions.GetProxy = c.opt.ProxyFunc
	}
	reqOptions.Headers = req.Header
	// Do request
	resp, respErr := c.Request(context.Background(), req.Method, req.URL.String(), reqOptions)
	defer func() {
		if respErr == nil && len(resp.Text()) > 0 {
			resp.CloseBody()
		}
	}()
	if respErr != nil {
		return nil, respErr
	}

	// Limit response body reading

	response := Response{
		Response: resp.Response(),
		Body:     resp.Content(),
		Request:  req,
	}

	return &response, nil
}

// doRequestChrome opens up a new chrome instance and makes request
func (c *Client) doRequestChrome(req *Request) (*Response, error) {
	// Set remote allocator or use local chrome instance
	var allocCtx context.Context
	var allocCancel context.CancelFunc
	if c.opt.RemoteAllocatorURL != "" {
		allocCtx, allocCancel = chromedp.NewRemoteAllocator(context.Background(), c.opt.RemoteAllocatorURL)
	} else {
		allocCtx, allocCancel = chromedp.NewExecAllocator(context.Background(), c.opt.AllocatorOptions...)
	}
	defer allocCancel()

	// Task context
	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	// Initiate default pre actions
	var body string
	var res *network.Response
	var defaultPreActions = []chromedp.Action{
		network.Enable(),
		network.SetExtraHTTPHeaders(ConvertHeaderToMap(req.Header)),
		chromedp.ActionFunc(func(ctx context.Context) error {
			chromedp.ListenTarget(ctx, func(ev interface{}) {
				if event, ok := ev.(*network.EventResponseReceived); ok {
					if res == nil && event.Type == "Document" {
						res = event.Response
					}
				}
			})
			return nil
		}),
		chromedp.Navigate(req.URL.String()),
		chromedp.WaitReady(":root"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			node, err := dom.GetDocument().Do(ctx)
			if err != nil {
				return err
			}
			body, err = dom.GetOuterHTML().WithNodeID(node.NodeID).Do(ctx)
			return err
		}),
	}

	// If options has pre actions, we override the default existing one.
	if len(c.opt.PreActions) != 0 {
		defaultPreActions = c.opt.PreActions
	}

	// Append custom actions to default ones.
	defaultPreActions = append(defaultPreActions, req.Actions...)

	// Run all actions
	if err := chromedp.Run(taskCtx, defaultPreActions...); err != nil {
		return nil, fmt.Errorf("request getting rendered: %w", err)
	}

	httpResponse := &http.Response{
		Request: req.Request,
	}

	// If response is set by default pre actions
	if res != nil {
		req.Header = ConvertMapToHeader(res.RequestHeaders)
		req.URL, _ = url.Parse(res.URL)
		httpResponse.StatusCode = int(res.Status)
		httpResponse.Proto = res.Protocol
		httpResponse.Header = ConvertMapToHeader(res.Headers)
	}

	response := Response{
		Response: httpResponse,
		Body:     []byte(body),
		Request:  req,
	}

	return &response, nil
}

// SetCookies handles the receipt of the cookies in a reply for the given URL
func (c *Client) SetClientCookies(URL string, cookies []*http.Cookie) error {

	formatUrl, err := url.Parse(URL)
	if err != nil {
		return err
	}
	c.SetCookies(formatUrl, cookies)
	return nil
}

// Cookies returns the cookies to send in a request for the given URL.
func (c *Client) Cookies(URL string) []*http.Cookie {
	return c.Cookies(URL)
}

// SetDefaultHeader sets header if not exists before
func SetDefaultHeader(header http.Header, key string, value string) http.Header {
	if header.Get(key) == "" {
		header.Set(key, value)
	}
	return header
}

// ConvertHeaderToMap converts http.Header to map[string]interface{}
func ConvertHeaderToMap(header http.Header) map[string]interface{} {
	m := make(map[string]interface{})
	for key, values := range header {
		for _, value := range values {
			m[key] = value
		}
	}
	return m
}

// ConvertMapToHeader converts map[string]interface{} to http.Header
func ConvertMapToHeader(m map[string]interface{}) http.Header {
	header := http.Header{}
	for k, v := range m {
		header.Set(k, v.(string))
	}
	return header
}

// NewRedirectionHandler returns maximum allowed redirection function with provided maxRedirect
func NewRedirectionHandler(maxRedirect int) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirect {
			return fmt.Errorf("stopped after %d redirects", maxRedirect)
		}
		return nil
	}
}
func gUnzipData(data []byte) (resData []byte, err error) {
	gz, _ := gzip.NewReader(bytes.NewReader(data))
	respBody, err := ioutil.ReadAll(gz)
	defer gz.Close()
	return respBody, err
}
func enflateData(data []byte) (resData []byte, err error) {
	zr, _ := zlib.NewReader(bytes.NewReader(data))
	defer zr.Close()
	enflated, err := ioutil.ReadAll(zr)
	return enflated, err
}
func unBrotliData(data []byte) (resData []byte, err error) {
	br := brotli.NewReader(bytes.NewReader(data))
	respBody, err := ioutil.ReadAll(br)
	return respBody, err
}

func DefaultChromeHeaders() *requests.OrderMap {
	headers := requests.NewOrderMap()
	headers.Set("cache-control", "no-cache")
	headers.Set("sec-ch-ua", `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`)
	headers.Set("sec-ch-ua-mobile", "?0")
	headers.Set("sec-ch-ua-platform", `"Windows"`)
	headers.Set("upgrade-insecure-requests", "1")
	headers.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.3")
	headers.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	headers.Set("sec-fetch-site", "same-origin")
	headers.Set("sec-fetch-mode", "navigate")
	headers.Set("sec-fetch-user", "?1")
	headers.Set("sec-fetch-dest", "document")
	headers.Set("accept-encoding", "gzip, deflate, br")
	headers.Set("accept-language", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	headers.Set("priority", "u=0, i")

	return headers
}

// DefaultFirefoxHeaders returns the default headers for Firefox.
//
// The headers returned are those commonly sent by Firefox on Windows.
func DefaultFirefoxHeaders() *requests.OrderMap {
	headers := requests.NewOrderMap()
	headers.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0")
	headers.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	headers.Set("accept-language", "en-US,en;q=0.5")
	headers.Set("accept-encoding", "gzip, deflate, br")
	headers.Set("upgrade-insecure-requests", "1")
	headers.Set("sec-fetch-dest", "document")
	headers.Set("sec-fetch-mode", "navigate")
	headers.Set("sec-fetch-site", "none")
	headers.Set("sec-fetch-user", "?1")
	headers.Set("priority", "u=0, i")
	headers.Set("te", "trailers")
	return headers
}

func DefaultChromeSpec() (ja3.Spec, ja3.H2Spec) {
	ja3Spec, ja3Err := ja3.CreateSpecWithStr("771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,51-27-17513-65037-16-65281-0-10-35-23-45-5-18-11-13-43,4588-29-23-24,0")
	if ja3Err != nil {
		panic(ja3Err)
	}
	h2String := "1:65536,2:0,4:6291456,6:262144|15663105|0|m,a,s,p"
	http2Spec, http2Err := ja3.CreateH2SpecWithStr(h2String)
	if http2Err != nil {
		panic(http2Err)
	}
	return ja3Spec, http2Spec
}

func DefaultFirefoxSpec() (ja3.Spec, ja3.H2Spec) {
	ja3Spec, ja3Err := ja3.CreateSpecWithStr("771,4865-4867-4866-49195-49199-52393-52392-49196-49200-49162-49161-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-34-51-43-13-45-28-27-65037,4588-29-23-24-25-256-257,0")
	if ja3Err != nil {
		panic(ja3Err)
	}
	http2Spec, http2Err := ja3.CreateH2SpecWithStr("1:65536,2:0,4:131072,5:16384|12517377|0|m,p,a,s")
	if http2Err != nil {
		panic(http2Err)
	}
	return ja3Spec, http2Spec
}
