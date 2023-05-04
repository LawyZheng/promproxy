// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package promproxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/LawyZheng/promproxy/internal/util"

	"github.com/cenkalti/backoff/v4"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func newScrapeCount() prometheus.Counter {
	return prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "promproxy_client_scrape_errors_total",
			Help: "Number of scrape errors",
		},
	)
}
func newPushCount() prometheus.Counter {
	return prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "promproxy_client_push_errors_total",
			Help: "Number of push errors",
		},
	)
}
func newPollCount() prometheus.Counter {
	return prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "promproxy_client_poll_errors_total",
			Help: "Number of poll errors",
		},
	)
}

func New(registerName, endpoint string, registry *prometheus.Registry) *Client {
	var register prometheus.Registerer
	var gatherer prometheus.Gatherer
	if registry != nil {
		register = registry
		gatherer = registry
	} else {
		register = prometheus.DefaultRegisterer
		gatherer = prometheus.DefaultGatherer
	}

	scrapeCount := newScrapeCount()
	pushCount := newPushCount()
	pollCount := newPollCount()
	register.MustRegister(scrapeCount, pushCount, pollCount)

	handler := promhttp.InstrumentMetricHandler(
		register, promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}),
	)

	return &Client{
		RegisterName: registerName,
		Endpoint:     endpoint,

		logger:           &defaultLogger{},
		httpClient:       http.DefaultClient,
		httpHandler:      handler,
		retryInitialWait: time.Second,
		retryMaxWait:     5 * time.Second,

		scrapeCount: scrapeCount,
		pushCount:   pushCount,
		pollCount:   pollCount,
	}
}

type Client struct {
	RegisterName string
	Endpoint     string

	retryInitialWait time.Duration
	retryMaxWait     time.Duration
	modifyRequest    func(r *http.Request) *http.Request
	logger           Logger
	httpClient       *http.Client
	httpHandler      http.Handler

	scrapeCount prometheus.Counter
	pushCount   prometheus.Counter
	pollCount   prometheus.Counter
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.modifyRequest != nil {
		req = c.modifyRequest(req)
	}
	return c.httpClient.Do(req)
}

func (c *Client) handleErr(request *http.Request, err error) error {
	c.scrapeCount.Inc()
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(err.Error())),
		Header:     http.Header{},
	}
	if err := c.doPush(resp, request); err != nil {
		c.pushCount.Inc()
		return errors.Wrap(err, "failed to push scrape response")
	}

	return err
}

func (c *Client) doScrape(request *http.Request) error {
	timeout, err := util.GetHeaderTimeout(request.Header)
	if err != nil {
		return c.handleErr(request, errors.Wrap(err, "get timeout error"))
	}

	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	request = request.WithContext(ctx)

	if request.URL.Hostname() != c.RegisterName {
		return c.handleErr(request, errors.New("scrape target doesn't match client register name"))
	}

	record := httptest.NewRecorder()
	c.ServeHTTP(record, request)

	if err = c.doPush(record.Result(), request); err != nil {
		c.pushCount.Inc()
		return errors.Wrap(err, "failed to push scrape response")
	}

	return nil
}

// Report the result of the scrape back up to the proxy.
func (c *Client) doPush(resp *http.Response, origRequest *http.Request) error {
	resp.Header.Set("id", origRequest.Header.Get("id")) // Link the request and response
	// Remaining scrape deadline.
	deadline, _ := origRequest.Context().Deadline()
	resp.Header.Set("X-Prometheus-Scrape-Timeout", fmt.Sprintf("%f", float64(time.Until(deadline))/1e9))

	base, err := url.Parse(c.Endpoint)
	if err != nil {
		return err
	}
	u, err := url.Parse("push")
	if err != nil {
		return err
	}
	url := base.ResolveReference(u)

	buf := &bytes.Buffer{}
	//nolint:errcheck
	resp.Write(buf)
	request := &http.Request{
		Method:        "POST",
		URL:           url,
		Body:          io.NopCloser(buf),
		ContentLength: int64(buf.Len()),
	}
	request = request.WithContext(origRequest.Context())
	if _, err = c.do(request); err != nil {
		return err
	}
	return nil
}

func (c *Client) doPoll() error {
	base, err := url.Parse(c.Endpoint)
	if err != nil {
		return errors.Wrap(err, "error parsing url")
	}
	u, err := url.Parse("poll")
	if err != nil {
		return errors.Wrap(err, "error parsing url poll")
	}
	url := base.ResolveReference(u)

	req, _ := http.NewRequest(http.MethodPost, url.String(), strings.NewReader(c.RegisterName))
	resp, err := c.do(req)
	if err != nil {
		return errors.Wrap(err, "error polling")
	}
	defer resp.Body.Close()

	request, err := http.ReadRequest(bufio.NewReader(resp.Body))
	if err != nil {
		return errors.Wrap(err, "error reading request")
	}
	c.logger.Info("get scrape request", request.Header.Get("id"), request.URL.Hostname())

	request.RequestURI = ""
	return c.doScrape(request)
}

func (c *Client) loop(ctx context.Context, bo backoff.BackOff) {
	op := func() error {
		return c.doPoll()
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := backoff.RetryNotify(op, bo, func(err error, _ time.Duration) {
				c.pollCount.Inc()
			}); err != nil {
				c.logger.Error(err)
			}
		}
	}
}

// ServeHTTP implement http.Handler interface
func (c *Client) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	c.httpHandler.ServeHTTP(rw, req)
}

func (c *Client) SetHTTPClient(client *http.Client) *Client {
	c.httpClient = client
	return c
}

func (c *Client) SetLogger(logger Logger) *Client {
	c.logger = logger
	return c
}

func (c *Client) SetModifyRequest(fn func(r *http.Request) *http.Request) *Client {
	c.modifyRequest = fn
	return c
}

func (c *Client) Run(ctx context.Context) {
	c.loop(ctx, newBackOffFromFlags(c.retryInitialWait, c.retryMaxWait))
}

func (c *Client) RunBackGround(ctx context.Context) {
	go func() {
		defer func() {
			if err := recover(); err != nil {
				c.logger.Error(fmt.Errorf("%v", err))
			}
		}()
		c.Run(ctx)
	}()
}

func newBackOffFromFlags(retryInitialWait, retryMaxWait time.Duration) backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = retryInitialWait
	b.Multiplier = 1.5
	b.MaxInterval = retryMaxWait
	b.MaxElapsedTime = time.Duration(0)
	return b
}
