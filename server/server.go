package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lawyzheng/lyhook"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/LawyZheng/promproxy/internal/util"
)

var (
	defaultLogger = logrus.StandardLogger()
)

type targetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
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

	coordinator Coordinator
	mux         http.Handler
	proxy       http.Handler
	logger      lyhook.Logger
}

func New(mux *http.ServeMux) *Server {
	logger := defaultLogger

	h := &Server{
		logger:      logger,
		coordinator: newCoordinator(logger),
		mux:         mux,

		DefaultScrapeTimeout: 15 * time.Second,
		MaxScrapeTimeout:     5 * time.Minute,
	}

	// api handlers
	handlers := map[string]http.HandlerFunc{
		"/push":    h.handlePush,
		"/poll":    h.handlePoll,
		"/clients": h.handleListClients,
		"/metrics": promhttp.Handler().ServeHTTP,
	}
	for path, handlerFunc := range handlers {
		counter := newHttpAPICounter().MustCurryWith(prometheus.Labels{"path": path})
		handler := promhttp.InstrumentHandlerCounter(counter, http.HandlerFunc(handlerFunc))
		histogram := newHttpPathHistogram().MustCurryWith(prometheus.Labels{"path": path})
		handler = promhttp.InstrumentHandlerDuration(histogram, handler)
		mux.Handle(path, handler)
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
	name, _ := io.ReadAll(r.Body)
	request, err := h.coordinator.WaitForScrapeInstruction(strings.TrimSpace(string(name)))
	if err != nil {
		h.logger.Error("Error WaitForScrapeInstruction: ", err)
		http.Error(w, fmt.Sprintf("Error WaitForScrapeInstruction: %s", err.Error()), 408)
		return
	}
	//nolint:errcheck
	request.WriteProxy(w) // Send full request as the body of the response.
	h.logger.WithFields(map[string]interface{}{
		"url":       request.URL.String(),
		"scrape_id": request.Header.Get("Id"),
	}).Info("Responded to /poll")
}

// handleListClients handles requests to list available clients as a JSON array.
func (h *Server) handleListClients(w http.ResponseWriter, r *http.Request) {
	known := h.coordinator.KnownClients()
	targets := make([]*targetGroup, 0, len(known))
	for _, k := range known {
		targets = append(targets, &targetGroup{Targets: []string{k}})
	}
	w.Header().Set("Content-Type", "application/json")
	//nolint:errcheck
	json.NewEncoder(w).Encode(targets)
	h.logger.WithField("client_count", len(known)).Info("msg", "Responded to /clients")
}

// handleProxy handles proxied scrapes from Prometheus.
func (h *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), util.GetScrapeTimeout(h.MaxScrapeTimeout, h.DefaultScrapeTimeout, r.Header))
	defer cancel()
	request := r.WithContext(ctx)
	request.RequestURI = ""

	resp, err := h.coordinator.DoScrape(ctx, request)
	if err != nil {
		h.logger.WithField("url", request.URL.String()).Error("Error scraping: ", err, "url")
		http.Error(w, fmt.Sprintf("Error scraping %q: %s", request.URL.String(), err.Error()), 500)
		return
	}
	defer resp.Body.Close()
	copyHTTPResponse(resp, w)
}

func (h *Server) SetLogger(logger lyhook.Logger) *Server {
	h.logger = logger
	return h
}

func (h *Server) SetCoordinator(coordinator Coordinator) *Server {
	h.coordinator = coordinator
	return h
}

// ServeHTTP discriminates between proxy requests (e.g. from Prometheus) and other requests (e.g. from the Client).
func (h *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host != "" { // Proxy request
		h.proxy.ServeHTTP(w, r)
	} else { // Non-proxy requests
		h.mux.ServeHTTP(w, r)
	}
}
