package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/lawyzheng/promproxy/internal/model"
	"github.com/lawyzheng/promproxy/internal/util"
)

var (
	defaultLogger = logrus.StandardLogger()
)

type targetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

type Router interface {
	Handle(pattern string, handler http.Handler)
	http.Handler
}

func newHttpAPICounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "promproxy_server_http_requests_total",
			Help: "Number of http api requests.",
		}, []string{"code", "path"},
	)
}

func newHttpProxyCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "promproxy_server_proxied_requests_total",
			Help: "Number of http proxy requests.",
		}, []string{"code"},
	)
}

func newHttpPathHistogram() *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "promproxy_server_http_duration_seconds",
			Help: "Time taken by path",
		}, []string{"path"})
}

func copyHTTPResponse(resp *http.Response, w http.ResponseWriter) {
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

type Server struct {
	DefaultScrapeTimeout time.Duration
	MaxScrapeTimeout     time.Duration

	rm          map[string]http.HandlerFunc
	clientAuth  func(r *http.Request) error
	promAuth    func(r *http.Request) error
	router      Router
	coordinator Coordinator
	proxy       http.Handler
	logger      logrus.FieldLogger
}

func New(router Router) *Server {
	logger := defaultLogger

	h := &Server{
		logger:      logger,
		coordinator: newCoordinator(logger),
		router:      router,

		DefaultScrapeTimeout: 15 * time.Second,
		MaxScrapeTimeout:     5 * time.Minute,
	}

	// api handlers
	h.rm = map[string]http.HandlerFunc{
		"/push":    h.handlePush,
		"/poll":    h.handlePoll,
		"/clients": h.handleListClients,
		"/metrics": promhttp.Handler().ServeHTTP,
	}
	for path, handlerFunc := range h.rm {
		counter := newHttpAPICounter().MustCurryWith(prometheus.Labels{"path": path})
		handler := promhttp.InstrumentHandlerCounter(counter, http.HandlerFunc(handlerFunc))
		histogram := newHttpPathHistogram().MustCurryWith(prometheus.Labels{"path": path})
		handler = promhttp.InstrumentHandlerDuration(histogram, handler)
		router.Handle(path, handler)
		counter.WithLabelValues("200")
		if path == "/push" {
			counter.WithLabelValues("500")
		}
		if path == "/poll" {
			counter.WithLabelValues("408")
		}
	}

	// proxy handler
	h.proxy = promhttp.InstrumentHandlerCounter(newHttpProxyCounter(), http.HandlerFunc(h.handleProxy))

	return h
}

func (h *Server) handleClientAuth(req *http.Request) error {
	if h.clientAuth == nil {
		return nil
	}

	return h.clientAuth(req)
}

func (h *Server) handlePromAuth(req *http.Request) error {
	if h.promAuth == nil {
		return nil
	}

	return h.promAuth(req)
}

// handlePush handles scrape responses from client.
func (h *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	buf := &bytes.Buffer{}
	io.Copy(buf, r.Body)
	scrapeResult, err := http.ReadResponse(bufio.NewReader(buf), nil)
	if err != nil {
		h.logger.Error("Error reading pushed response: ", err)
		http.Error(w, fmt.Sprintf("Error pushing: %s", err.Error()), 500)
		return
	}
	scrapeId := scrapeResult.Header.Get("Id")
	h.logger.WithField("scrape_id", scrapeId).Info("Got /push")

	if err := h.handleClientAuth(r); err != nil {
		h.logger.WithField("scrape_id", scrapeId).Error("Auth Failed: ", err)
		http.Error(w, fmt.Sprintf("Auth failed: %s", err), http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), util.GetScrapeTimeout(h.MaxScrapeTimeout, h.DefaultScrapeTimeout, scrapeResult.Header))
	defer cancel()

	err = h.coordinator.ScrapeResult(ctx, scrapeResult)
	if err != nil {
		h.logger.WithField("scrape_id", scrapeId).Error("Error pushing: ", err)
		http.Error(w, fmt.Sprintf("Error pushing: %s", err.Error()), 500)
	}
}

// handlePoll handles clients registering and asking for scrapes.
func (h *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	defer r.Body.Close()

	client := model.ClientPollRequest{}
	if err := json.Unmarshal(b, &client); err != nil {
		h.logger.Error("unmarshal failed: ", err)
		http.Error(w, fmt.Sprintf("unmarshal failed: %s", err), http.StatusBadRequest)
		return
	}

	if err := h.handleClientAuth(r); err != nil {
		h.logger.WithField("name", client.Name).Error("Auth Failed: ", err)
		http.Error(w, fmt.Sprintf("Auth failed: %s", err), http.StatusUnauthorized)
		return
	}

	request, err := h.coordinator.WaitForScrapeInstruction(client)
	if err != nil {
		h.logger.Error("Error WaitForScrapeInstruction: ", err)
		http.Error(w, fmt.Sprintf("Error WaitForScrapeInstruction: %s", err.Error()), 408)
		return
	}
	//nolint:errcheck
	request.WriteProxy(w) // Send full request as the body of the response.
	h.logger.WithFields(map[string]interface{}{
		"name":      request.URL.Hostname(),
		"scrape_id": request.Header.Get("Id"),
	}).Info("Responded to /poll")
}

// handleListClients handles requests to list available clients as a JSON array.
func (h *Server) handleListClients(w http.ResponseWriter, r *http.Request) {
	if err := h.handlePromAuth(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	known := h.coordinator.KnownClients()
	targets := make([]*targetGroup, 0, len(known))
	for _, k := range known {
		targets = append(targets, &targetGroup{
			Targets: []string{k.GetName()},
			Labels:  k.GetLabels(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	//nolint:errcheck
	json.NewEncoder(w).Encode(targets)
	h.logger.WithField("client_count", len(known)).Info("msg", "Responded to /clients")
}

// handleProxy handles proxied scrapes from Prometheus.
func (h *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if err := h.handlePromAuth(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), util.GetScrapeTimeout(h.MaxScrapeTimeout, h.DefaultScrapeTimeout, r.Header))
	defer cancel()
	request := r.WithContext(ctx)
	request.RequestURI = ""

	resp, err := h.coordinator.DoScrape(ctx, request)
	if err != nil {
		h.logger.WithField("name", request.URL.Hostname()).Error("Error scraping: ", err)
		http.Error(w, fmt.Sprintf("Error scraping %q: %s", request.URL.Hostname(), err.Error()), 500)
		return
	}
	defer resp.Body.Close()
	copyHTTPResponse(resp, w)
}

func (h *Server) SetPromAuth(fn func(r *http.Request) error) *Server {
	h.promAuth = fn
	return h
}

func (h *Server) SetClientAuth(fn func(r *http.Request) error) *Server {
	h.clientAuth = fn
	return h
}

func (h *Server) SetLogger(logger logrus.FieldLogger) *Server {
	h.logger = logger
	h.coordinator.SetLogger(logger)
	return h
}

func (h *Server) SetCoordinator(coordinator Coordinator) *Server {
	h.coordinator = coordinator
	return h
}

func (h *Server) ProxyHandler() http.Handler {
	return h.proxy
}

func (h *Server) RouterHandler() map[string]http.HandlerFunc {
	return h.rm
}

func (h *Server) GetKnownClients() []PollClient {
	return h.coordinator.KnownClients()
}

// ServeHTTP discriminates between proxy requests (e.g. from Prometheus) and other requests (e.g. from the Client).
func (h *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host != "" { // Proxy request
		h.proxy.ServeHTTP(w, r)
	} else { // Non-proxy requests
		h.router.ServeHTTP(w, r)
	}
}
