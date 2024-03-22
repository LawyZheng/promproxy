package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

	"github.com/lawyzheng/promproxy/client"
)

func createMetric() prometheus.Counter {
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_metrics",
		Help: "metrics for testing",
	})
	go func() {
		for {
			counter.Add(1)
			time.Sleep(1 * time.Second)
		}
	}()
	return counter
}

// use prometheus default registry
func clientWithDefaultRegistry() error {
	prometheus.MustRegister(createMetric())
	c := client.New("client_with_default", "http://localhost:8080", nil)
	return <-c.RunBackGround(context.Background())
}

// define you own registry
func clientWithCustomRegistry() error {
	myRegistry := prometheus.NewRegistry()
	myRegistry.MustRegister(createMetric())
	c := client.New("client_with_custom", "http://localhost:8080", myRegistry)
	return <-c.RunBackGround(context.Background())
}

func main() {
	g := new(errgroup.Group)

	g.Go(clientWithDefaultRegistry)
	g.Go(clientWithCustomRegistry)

	if err := g.Wait(); err != nil {
		panic(err)
	}
}
