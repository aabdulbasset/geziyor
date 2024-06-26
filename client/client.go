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
	"github.com/imroc/req/v3"
)

var (
	// ErrNoCookieJar is the error type for missing cookie jar
	ErrNoCookieJar = errors.New("cookie jar is not available")
)

// Client is a small wrapper around *http.Client to provide new methods.
type Client struct {
	*req.Client
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
	ProxyFunc             func(*http.Request) (*url.URL, error)
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
	// Default proxy function is http.ProxyFunction
	var proxyFunction = http.ProxyFromEnvironment
	if opt.ProxyFunc != nil {
		proxyFunction = opt.ProxyFunc
	}

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
	httpClient := req.C()
	httpClient.SetTimeout(180 * time.Second).SetIdleConnTimeout(15 * time.Second).SetMaxConnsPerHost(1000).SetMaxIdleConns(0)
	httpClient.SetTLSHandshakeTimeout(10 * time.Second).SetExpectContinueTimeout(2 * time.Second).SetProxy(proxyFunction)

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
	// Do request
	clonedClient := c.Clone()
	request := clonedClient.R()
	request.RawRequest = req.Request
	if req.Header.Get("user-agent") != "" {
		userAgent := req.Header.Get("user-agent")
		if strings.Contains(strings.ToLower(userAgent), "version") {
			clonedClient.ImpersonateSafari()
		} else if strings.Contains(strings.ToLower(userAgent), "firefox") {
			clonedClient.ImpersonateFirefox()
		} else {
			clonedClient.ImpersonateChrome()
		}
	}
	request.Headers = req.Header
	resp, err := request.Send(req.Method, req.URL.String())
	defer func() {
		if resp.Err == nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()
	if err != nil || resp.Err != nil {
		return nil, fmt.Errorf("response: %w", err)
	}

	// Limit response body reading

	encoding := resp.Header["Content-Encoding"]
	body, err := ioutil.ReadAll(resp.Body)
	var finalres []byte
	if err != nil {
		panic(err)
	}
	finalres = body
	if len(encoding) > 0 {
		if encoding[0] == "gzip" {
			unz, err := gUnzipData(body)
			if err != nil {
				panic(err)
			}
			finalres = unz
		} else if encoding[0] == "deflate" {
			unz, err := enflateData(body)
			if err != nil {
				panic(err)
			}
			finalres = unz
		} else if encoding[0] == "br" {
			unz, err := unBrotliData(body)
			if err != nil {
				panic(err)
			}
			finalres = unz
		} else {
			fmt.Println("UNKNOWN ENCODING: " + encoding[0])
			finalres = body
		}
	} else {
		finalres = body
	}

	response := Response{
		Response: resp.Response,
		Body:     finalres,
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

	c.SetCommonCookies()
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
