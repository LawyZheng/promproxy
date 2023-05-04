package client

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestClient_Run(t *testing.T) {
	type fields struct {
		RegisterName string
		Endpoint     string
	}
	type args struct {
		ctx    context.Context
		logger Logger
	}
	tests := []struct {
		name   string
		fields fields
		args   args
	}{
		{
			"test",
			fields{
				RegisterName: "client",
				Endpoint:     "http://localhost:8080",
			},
			args{
				ctx:    context.Background(),
				logger: nil,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test := prometheus.NewCounter(prometheus.CounterOpts{
				Name: "test_metrics",
				Help: "metrics for testing",
			})
			prometheus.MustRegister(test)

			c := New(tt.fields.RegisterName, tt.fields.Endpoint, nil)
			ctx, cancel := context.WithTimeout(tt.args.ctx, 3*time.Second)
			go c.Run(ctx)

			for {
				test.Inc()
				time.Sleep(time.Second)
			}
			cancel()

		})
	}
}
