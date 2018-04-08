package dnstohttps

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"time"

	"blitiri.com.ar/go/dnss/internal/dnsjson"
	"blitiri.com.ar/go/dnss/internal/dnsserver"

	"github.com/golang/glog"
	"github.com/miekg/dns"
	"golang.org/x/net/trace"
)

// httpsResolver implements the dnsserver.Resolver interface by querying a
// server via DNS-over-HTTPS.
//
// It supports two modes: JSON (like https://dns.google.com) and DoH
// (https://tools.ietf.org/html/draft-ietf-doh-dns-over-https-05).
type httpsResolver struct {
	Upstream *url.URL
	CAFile   string
	client   *http.Client
	mode     string
}

func loadCertPool(caFile string) (*x509.CertPool, error) {
	pemData, err := ioutil.ReadFile(caFile)
	if err != nil {
		return nil, err
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("Error appending certificates")
	}

	return pool, nil
}

// NewJSONResolver creates a new JSON resolver which uses the given upstream
// URL to resolve queries.
func NewJSONResolver(upstream *url.URL, caFile string) *httpsResolver {
	return &httpsResolver{
		Upstream: upstream,
		CAFile:   caFile,
		mode:     "JSON",
	}
}

// NewDoHResolver creates a new DoH resolver, which uses the given upstream
// URL to resolve queries.
func NewDoHResolver(upstream *url.URL, caFile string) *httpsResolver {
	return &httpsResolver{
		Upstream: upstream,
		CAFile:   caFile,
		mode:     "DoH",
	}
}

func (r *httpsResolver) Init() error {
	transport := &http.Transport{
		// Take the semi-standard proxy settings from the environment.
		Proxy: http.ProxyFromEnvironment,
	}

	r.client = &http.Client{
		// Give our HTTP requests 4 second timeouts: DNS usually doesn't wait
		// that long anyway, but this helps with slow connections.
		Timeout: 4 * time.Second,

		Transport: transport,
	}

	// If CAFile is empty, we're ok with the defaults (use the system default
	// CA database).
	if r.CAFile == "" {
		return nil
	}

	pool, err := loadCertPool(r.CAFile)
	if err != nil {
		return err
	}

	transport.TLSClientConfig = &tls.Config{
		ClientCAs: pool,
	}

	return nil
}

func (r *httpsResolver) Maintain() {
}

func (r *httpsResolver) Query(req *dns.Msg, tr trace.Trace) (*dns.Msg, error) {
	if r.mode == "DoH" {
		return r.queryDoH(req, tr)
	}
	return r.queryJSON(req, tr)
}

func (r *httpsResolver) queryDoH(req *dns.Msg, tr trace.Trace) (*dns.Msg, error) {
	packed, err := req.Pack()
	if err != nil {
		return nil, fmt.Errorf("cannot pack query: %v", err)
	}

	if glog.V(3) {
		tr.LazyPrintf("DoH POST %v", r.Upstream)
	}

	// TODO: Accept header.

	hr, err := r.client.Post(
		r.Upstream.String(),
		"application/dns-udpwireformat",
		bytes.NewReader(packed))
	if err != nil {
		return nil, fmt.Errorf("POST failed: %v", err)
	}
	tr.LazyPrintf("%s  %s", hr.Proto, hr.Status)
	defer hr.Body.Close()

	if hr.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Response status: %s", hr.Status)
	}

	// Read the HTTPS response, and parse the message.
	ct, _, err := mime.ParseMediaType(hr.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse content type: %v", err)
	}

	if ct != "application/dns-udpwireformat" {
		return nil, fmt.Errorf("unknown response content type %q", ct)
	}

	respRaw, err := ioutil.ReadAll(io.LimitReader(hr.Body, 4092))
	if err != nil {
		return nil, fmt.Errorf("error reading from body: %v", err)
	}

	respDNS := &dns.Msg{}
	err = respDNS.Unpack(respRaw)
	if err != nil {
		return nil, fmt.Errorf("error unpacking response: %v", err)
	}

	return respDNS, nil
}

func (r *httpsResolver) queryJSON(req *dns.Msg, tr trace.Trace) (*dns.Msg, error) {
	// Only answer single-question queries.
	// In practice, these are all we get, and almost no server supports
	// multi-question requests anyway.
	if len(req.Question) != 1 {
		return nil, fmt.Errorf("multi-question query")
	}

	question := req.Question[0]
	// Only answer IN-class queries, which are the ones used in practice.
	if question.Qclass != dns.ClassINET {
		return nil, fmt.Errorf("query class != IN")
	}

	// Build the query and send the request.
	url := *r.Upstream
	vs := url.Query()
	vs.Set("name", question.Name)
	vs.Set("type", dns.TypeToString[question.Qtype])
	url.RawQuery = vs.Encode()
	// TODO: add random_padding.

	if glog.V(3) {
		tr.LazyPrintf("JSON GET %v", url)
	}

	hr, err := r.client.Get(url.String())
	if err != nil {
		return nil, fmt.Errorf("GET failed: %v", err)
	}
	tr.LazyPrintf("%s  %s", hr.Proto, hr.Status)
	defer hr.Body.Close()

	if hr.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Response status: %s", hr.Status)
	}

	// Read the HTTPS response, and parse the JSON.
	body, err := ioutil.ReadAll(hr.Body)
	if err != nil {
		return nil, fmt.Errorf("Failed to read body: %v", err)
	}

	jr := &dnsjson.Response{}
	err = json.Unmarshal(body, jr)
	if err != nil {
		return nil, fmt.Errorf("Failed to unmarshall: %v", err)
	}

	if len(jr.Question) != 1 {
		return nil, fmt.Errorf("Wrong number of questions in the response")
	}

	// Build the DNS response.
	resp := &dns.Msg{
		MsgHdr: dns.MsgHdr{
			Id:       req.Id,
			Response: true,
			Opcode:   req.Opcode,
			Rcode:    jr.Status,

			Truncated:          jr.TC,
			RecursionDesired:   jr.RD,
			RecursionAvailable: jr.RA,
			AuthenticatedData:  jr.AD,
			CheckingDisabled:   jr.CD,
		},
		Question: []dns.Question{
			{
				Name:   jr.Question[0].Name,
				Qtype:  jr.Question[0].Type,
				Qclass: dns.ClassINET,
			}},
	}

	for _, answer := range jr.Answer {
		// TODO: This "works" but is quite hacky. Is there a better way,
		// without doing lots of data parsing?
		s := fmt.Sprintf("%s %d IN %s %s",
			answer.Name, answer.TTL,
			dns.TypeToString[answer.Type], answer.Data)
		rr, err := dns.NewRR(s)
		if err != nil {
			return nil, fmt.Errorf("Error parsing answer: %v", err)
		}

		resp.Answer = append(resp.Answer, rr)
	}

	return resp, nil
}

// Compile-time check that the implementation matches the interface.
var _ dnsserver.Resolver = &httpsResolver{}
