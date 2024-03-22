# Promproxy 

Promproxy is forked from [PushProx](https://github.com/prometheus-community/PushProx), which is a client and proxy that allows transversing of NAT and other
similar network topologies by Prometheus, while still following the pull model.

However, unlike PushProx includes two applications (proxy and client), promproxy just provides a Go SDK package, so you can implement your own server and client by yourself.

It's NOT AN ALTERNATIVE of PushProx. But you can use it more convenient if you're in the Go stack because you just need to add two lines in your code and everything will start.

While this is reasonably robust in practice, this is a work in progress.

Compared with PushProx, promproxy is:

1. Less network chain(client is embedded by code) and component(only need a server)
2. Self-defined authentication supported


## Usage

### Installation
```shell
go get github.com/lawyzheng/promproxy
```

### Implement the server (function the same as proxy in PushProx)
```go
func main() {
	// any router engine you like
	mux := http.NewServeMux()
	s := server.New(mux)

	http.ListenAndServe(":8080", s)
}
```
Or, you can also embed it with [gin](github.com/gin-gonic/gin) or other HTTP servers.

### Embed the client into your application
```go
...

prometheus.MustRegister(createMetric())
c := client.New("client_with_default", "http://localhost:8080", nil)
<-c.RunBackGround(context.Background())

...
```
You can see more examples [here](./example/client/main.go).


## Config in Prometheus
In Prometheus, use the proxy as a `proxy_url`, server-side will also provide a `http_sd_configs` for the scrape target:
```yaml
scrape_configs:
  - job_name: 'proxy'
    proxy_url: http://<server_ip_address>:<server_port>
    http_sd_configs:
      - url: http://<server_ip_address>:<server_port>/clients
```

If the target must be scraped over SSL/TLS, add:
```yaml
  params:
    _scheme: [https]
```
rather than the usual `scheme: https`. Only the default `scheme: http` works with the proxy,
so this workaround is required.

## Security

### Server-Client
On the client side, you can use `client.SetModifyRequest()` to enable any authentication logic by HTTP requests. But you need to AVOID EMBEDDING YOUR AUTH LOGIC IN THE REQUEST BODY!!! Meanwhile, on the server side, you can use `server.SetClientAuth()` to verify the request from the client.


### Server-Prometheus
On the server side, you can use `server.SetPromAuth()` to verify the request from Prometheus.
