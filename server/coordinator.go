package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

func newClientGauge() prometheus.Gauge {
	return prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "promproxy_server_clients",
			Help: "Number of known pushprox clients.",
		},
	)
}

type knownClient struct {
	PollClient
	expire time.Time
}

type PollClient interface {
	GetName() string
	GetLabels() map[string]string
}

type Coordinator interface {
	DoScrape(ctx context.Context, r *http.Request) (*http.Response, error)
	WaitForScrapeInstruction(client PollClient) (*http.Request, error)
	ScrapeResult(ctx context.Context, r *http.Response) error
	KnownClients() []PollClient
}

// coordinator for scrape requests and responses
type coordinator struct {
	mu sync.Mutex

	// Clients waiting for a scrape.
	waiting map[string]chan *http.Request
	// Responses from clients.
	responses map[string]chan *http.Response
	// Clients we know about and when they last contacted us.
	known map[string]knownClient

	logger              *logrus.Logger
	clientCounter       prometheus.Gauge
	registrationTimeout time.Duration
}

// newCoordinator initiates the coordinator and starts the client cleanup routine
func newCoordinator(logger *logrus.Logger) Coordinator {
	counter := newClientGauge()
	prometheus.MustRegister(counter)

	c := &coordinator{
		waiting:   map[string]chan *http.Request{},
		responses: map[string]chan *http.Response{},
		known:     map[string]knownClient{},

		logger:              logger,
		clientCounter:       counter,
		registrationTimeout: 5 * time.Minute,
	}

	go c.gc()
	return c
}

// Generate a unique ID
func (c *coordinator) genID() (string, error) {
	id, err := uuid.NewRandom()
	return id.String(), err
}

func (c *coordinator) getRequestChannel(fqdn string) chan *http.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.waiting[fqdn]
	if !ok {
		ch = make(chan *http.Request)
		c.waiting[fqdn] = ch
	}
	return ch
}

func (c *coordinator) getResponseChannel(id string) chan *http.Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.responses[id]
	if !ok {
		ch = make(chan *http.Response)
		c.responses[id] = ch
	}
	return ch
}

// Remove a response channel. Idempotent.
func (c *coordinator) removeResponseChannel(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.responses, id)
}

// DoScrape requests a scrape.
func (c *coordinator) DoScrape(ctx context.Context, r *http.Request) (*http.Response, error) {
	id, err := c.genID()
	if err != nil {
		return nil, err
	}
	c.logger.WithFields(map[string]interface{}{
		"scrape_id": id,
		"name":      r.URL.Hostname(),
	}).Info("DoScrape")
	r.Header.Add("Id", id)
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout reached for %q: %s", r.URL.Hostname(), ctx.Err())
	case c.getRequestChannel(r.URL.Hostname()) <- r:
	}

	respCh := c.getResponseChannel(id)
	defer c.removeResponseChannel(id)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-respCh:
		return resp, nil
	}
}

// WaitForScrapeInstruction registers a client waiting for a scrape result
func (c *coordinator) WaitForScrapeInstruction(client PollClient) (*http.Request, error) {
	c.logger.WithField("name", client.GetName()).Info("WaitForScrapeInstruction")

	c.addKnownClient(client)
	// TODO: What if the client times out?
	ch := c.getRequestChannel(client.GetName())

	// exhaust existing poll request (eg. timeouted queues)
	select {
	case ch <- nil:
		//
	default:
		break
	}

	for {
		request := <-ch
		if request == nil {
			return nil, fmt.Errorf("request is expired")
		}

		select {
		case <-request.Context().Done():
			// Request has timed out, get another one.
		default:
			return request, nil
		}
	}
}

// ScrapeResult send by client
func (c *coordinator) ScrapeResult(ctx context.Context, r *http.Response) error {
	id := r.Header.Get("Id")
	c.logger.WithField("scrape_id", id).Info("msg", "ScrapeResult")
	// Don't expose internal headers.
	r.Header.Del("Id")
	r.Header.Del("X-Prometheus-Scrape-Timeout-Seconds")
	select {
	case c.getResponseChannel(id) <- r:
		return nil
	case <-ctx.Done():
		c.removeResponseChannel(id)
		return ctx.Err()
	}
}

func (c *coordinator) addKnownClient(client PollClient) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.known[client.GetName()] = knownClient{
		PollClient: client,
		expire:     time.Now(),
	}
	c.clientCounter.Set(float64(len(c.known)))
}

// KnownClients returns a list of alive clients
func (c *coordinator) KnownClients() []PollClient {
	c.mu.Lock()
	defer c.mu.Unlock()

	limit := time.Now().Add(-c.registrationTimeout)
	known := make([]PollClient, 0, len(c.known))
	for _, t := range c.known {
		if limit.Before(t.expire) {
			known = append(known, t)
		}
	}
	return known
}

// Garbagee collect old clients.
func (c *coordinator) gc() {
	for range time.Tick(1 * time.Minute) {
		func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			limit := time.Now().Add(-c.registrationTimeout)
			deleted := 0
			for k, ts := range c.known {
				if ts.expire.Before(limit) {
					delete(c.known, k)
					deleted++
				}
			}
			c.logger.WithFields(map[string]interface{}{
				"deleted":   deleted,
				"remaining": len(c.known),
			}).Info("GC of clients completed")
			c.clientCounter.Set(float64(len(c.known)))
		}()
	}
}
