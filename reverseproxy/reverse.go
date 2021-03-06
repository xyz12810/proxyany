// fork from https://github.com/cssivision/reverseproxy
// updated from https://golang.org/src/net/http/httputil/reverseproxy.go
package reverseproxy

import (
	"context"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTimeout = time.Minute * 5
)

// ReverseProxy is an HTTP Handler that takes an incoming request and
// sends it to another server, proxying the response back to the
// client, support http, also support https tunnel using http.hijacker
type ReverseProxy struct {
	// Set the timeout of the proxy server, default is 5 minutes
	Timeout time.Duration

	// Director must be a function which modifies
	// the request into a new request to be sent
	// using Transport. Its response is then copied
	// back to the original client unmodified.
	// Director must not access the provided Request
	// after returning.
	Director func(*http.Request, *DomainMapping)

	// The transport used to perform proxy requests.
	// default is http.DefaultTransport.
	Transport http.RoundTripper

	// FlushInterval specifies the flush interval
	// to flush to the client while copying the
	// response body. If zero, no periodic flushing is done.
	FlushInterval time.Duration

	// ErrorLog specifies an optional logger for errors
	// that occur when attempting to proxy the request.
	// If nil, logging goes to os.Stderr via the log package's
	// standard logger.
	ErrorLog *log.Logger

	MapGroup MapGroup
}

// NewReverseProxy returns a new ReverseProxy that routes
// URLs to the scheme, host, and base path provided in target. If the
// target's path is "/base" and the incoming request was for "/dir",
// the target request will be for /base/dir. if the target's query is a=10
// and the incoming request's query is b=100, the target's request's query
// will be a=10&b=100.
// NewReverseProxy does not rewrite the Host header.
// To rewrite Host headers, use ReverseProxy directly with a custom
// Director policy.
func NewReverseProxy(mapGroup *MapGroup) *ReverseProxy {
	director := func(req *http.Request, mapping *DomainMapping) {
		// 1. req.URL
		// scheme
		req.URL.Scheme = mapping.Target.Scheme

		// host, specific the low level tcp connection target
		req.URL.Host = mapping.ReplaceStr(req.Host)

		// path
		req.URL.Path = singleJoiningSlash(mapping.Target.Path, req.URL.Path)

		// query
		targetQuery := mapping.Target.RawQuery
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
		req.URL.RawQuery = mapping.ReplaceStr(req.URL.RawQuery)

		// 2. req.Host, specific the http request content, aka "Host" header
		// If Host is empty, the Request.Write method uses the value of URL.Host.
		req.Host = req.URL.Host // force use URL.Host

		// 3. req.Header
		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/65.0.3325.181 Safari/537.36")
		}

		// 4. compression
		// We also try to compress upstream communication
		req.Header.Set("accept-encoding", "gzip")
	}

	transport := &http.Transport{
		// disable comression, we will set it later manully
		DisableCompression:    true,
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
	}
	return &ReverseProxy{Director: director, Transport: transport, MapGroup: *mapGroup}
}

func (p *ReverseProxy) ProxyHTTP(rw http.ResponseWriter, req *http.Request) {
	// get domain mapping
	mapping := p.MapGroup.GetMapping(req.Host)
	if mapping == nil {
		log.Printf("can't find mapping for %v\n", req.Host)
		return
	}

	ctx := req.Context()

	if cn, ok := rw.(http.CloseNotifier); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		notifyChan := cn.CloseNotify()
		go func() {
			select {
			case <-notifyChan:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	// outreq to upstream:

	outreq := req.WithContext(ctx) // includes shallow copies of maps, but okay
	if req.ContentLength == 0 {
		outreq.Body = nil // Issue 16036: nil Body for http.Transport retries
	}

	copyHeader(outreq.Header, req.Header, nil)

	p.Director(outreq, mapping)
	outreq.Close = false

	// Remove hop-by-hop headers listed in the "Connection" header, Remove hop-by-hop headers.
	removeHeaders(outreq.Header)

	// replace domain in headers
	mapping.ReplaceHeader(&outreq.Header)

	// Add X-Forwarded-For Header.
	addXForwardedForHeader(outreq)

	// 2. do request part

	log.Println("requesting...", outreq.Method, outreq.URL)
	res, err := p.Transport.RoundTrip(outreq)
	if err != nil {
		p.logf("http: proxy error 1: %v", err)
		rw.WriteHeader(http.StatusBadGateway)
		return
	}

	// 3. response part

	// Remove hop-by-hop headers listed in the "Connection" header of the response, Remove hop-by-hop headers.
	removeHeaders(res.Header)
	// replace domain in headers reversely
	for _, mp := range p.MapGroup.maps {
		mp.Reverse().ReplaceHeader(&res.Header)
	}

	// Copy header from response to client.
	copyHeader(rw.Header(), res.Header, &[]string{"content-length", "content-encoding"})

	// add Access-Control-Allow-Origin
	rw.Header().Set("Access-Control-Allow-Origin", "*")

	// The "Trailer" header isn't included in the Transport's response, Build it up from Trailer.
	if len(res.Trailer) > 0 {
		trailerKeys := make([]string, 0, len(res.Trailer))
		for k := range res.Trailer {
			trailerKeys = append(trailerKeys, k)
		}
		rw.Header().Add("Trailer", strings.Join(trailerKeys, ", "))
	}

	rw.WriteHeader(res.StatusCode)

	// gzip part:

	// We are ignoring any q-value here, so this is wrong for the case q=0
	clientAE := req.Header.Get("Accept-Encoding")
	clientAcceptsGzip := strings.Contains(clientAE, "gzip")

	rw.Header().Set("Transfer-Encoding", "chunked")
	if clientAcceptsGzip {
		// disable gzip because of gzip bug
		//rw.Header().Set("Transfer-Encoding", "gzip")
	}

	// decompress and compress
	r, w, err := HandleCompression(res, rw, clientAcceptsGzip)
	p.rewriteBody(w, r)

	// trailer part:

	if len(res.Trailer) > 0 {
		// Force chunking if we saw a response trailer.
		// This prevents net/http from calculating the length for short
		// bodies and adding a Content-Length.
		if fl, ok := rw.(http.Flusher); ok {
			fl.Flush()
		}
	}

	// close now, instead of defer, to populate res.Trailer
	res.Body.Close()
	copyHeader(rw.Header(), res.Trailer, nil)
}

func (p *ReverseProxy) ProxyHTTPS(rw http.ResponseWriter, req *http.Request) {
	hij, ok := rw.(http.Hijacker)
	if !ok {
		p.logf("http server does not support hijacker")
		return
	}

	clientConn, _, err := hij.Hijack()
	if err != nil {
		p.logf("http: proxy error 3: %v", err)
		return
	}

	proxyConn, err := net.Dial("tcp", req.URL.Host)
	if err != nil {
		p.logf("http: proxy error 4: %v", err)
		return
	}

	// The returned net.Conn may have read or write deadlines
	// already set, depending on the configuration of the
	// Server, to set or clear those deadlines as needed
	// we set timeout to 5 minutes
	deadline := time.Now()
	if p.Timeout == 0 {
		deadline = deadline.Add(time.Minute * 5)
	} else {
		deadline = deadline.Add(p.Timeout)
	}

	err = clientConn.SetDeadline(deadline)
	if err != nil {
		p.logf("http: proxy error 5: %v", err)
		return
	}

	err = proxyConn.SetDeadline(deadline)
	if err != nil {
		p.logf("http: proxy error 6: %v", err)
		return
	}

	_, err = clientConn.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
	if err != nil {
		p.logf("http: proxy error 7: %v", err)
		return
	}

	go func() {
		io.Copy(clientConn, proxyConn)
		clientConn.Close()
		proxyConn.Close()
	}()

	io.Copy(proxyConn, clientConn)
	proxyConn.Close()
	clientConn.Close()
}

func (p *ReverseProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	log.Println(req.RemoteAddr, req.Method, req.Host, req.URL)
	if req.Method == "CONNECT" {
		p.ProxyHTTPS(rw, req)
	} else {
		p.ProxyHTTP(rw, req)
	}
}

func (p *ReverseProxy) rewriteBody(dst io.Writer, src io.Reader) {
	bodyData, err := ioutil.ReadAll(src)

	if err == nil {
		if len(bodyData) > 0 {
			for _, mapping := range p.MapGroup.maps {
				bodyData = mapping.Reverse().ReplaceBytes(bodyData)
			}
		}
	} else {
		log.Printf("read body error: %v\n", err)
		// Work around the closed-body-on-redirect bug in the runtime
		// https://github.com/golang/go/issues/10069
		bodyData = make([]byte, 0)
	}
	//fmt.Println(string(bodyData[0:50]))

	written, err := dst.Write(bodyData)
	if err != nil || written != len(bodyData) || len(bodyData) == 0 {
		if err != nil && err.Error() == "http: request method or response status code does not allow body" {
			return
		}
		p.logf("rewrite body error: %v, %v/%v", err, len(bodyData), written)
	}
}

func (p *ReverseProxy) logf(format string, args ...interface{}) {
	if p.ErrorLog != nil {
		p.ErrorLog.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}
